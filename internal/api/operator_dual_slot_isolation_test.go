package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Dual-slot poll isolation (Prism#740, Bastion clearance finding on #392).
//
// #392's join (engineBArmedAwaits, operator.go) is the first thing that lets
// GET /runtime/{blueprint_id}/pending and GET /cockpit/contracts serve a
// POPULATED awaits facet at all. Every existing fixture that already
// exercises both slots together (TestOperator_CallSlotIsolationBothWays,
// TestCockpit_EngineBPreviewArmedAwaitDoesNotLeakToAntenna) leaves the OTHER
// slot's AWAITS empty — its program either declares none, or declares one
// that on-start never arms. A routing bug that swapped, duplicated, or
// hardcoded the slot argument on the read path would still answer back an
// empty list on that leg, which reads identically to "this slot legitimately
// has nothing armed" — the exact ambiguity the bail calls out. These tests
// instead arm a DIFFERENTLY NAMED await on BOTH slots at the same time, so a
// swapped/duplicated slot surfaces a name the polled leg must never produce,
// not just a non-empty list.

// TestOperator_PendingIsolatesArmedAwaitsPerSlot proves GET
// /runtime/{blueprint_id}/pending's isolation: with SlotOnAir and
// SlotPreview both hosting a distinct program with its own armed await
// simultaneously, the antenna leg (no ?target) reports ONLY the on-air
// await, and ?target=preview reports ONLY the preview await — never the
// other's, never both. engineBSlot (operator.go) is the single decision
// point getRuntimePending's Engine B leg goes through (line ~444,
// engineBArmedAwaits(host, engineBSlot(r), ...)) — this is the leg the bail
// targets by name.
func TestOperator_PendingIsolatesArmedAwaitsPerSlot(t *testing.T) {
	onAirProgram := buildEngineBOperatorProgram(t, "call-onair", "called-onair",
		"pick-onair", "core.primitive.integer", "picked-onair")
	previewProgram := buildEngineBOperatorProgram(t, "call-preview", "called-preview",
		"pick-preview", "core.primitive.integer", "picked-preview")

	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotOnAir, onAirProgram)
	f.takeSlot(t, bluehost.SlotPreview, previewProgram)

	assertOnlyAwait := func(t *testing.T, path, wantName string) {
		t.Helper()
		w := opRequest(t, f.mux, "GET", path, "operator", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("pending %s: got %d, want 200 (body=%s)", path, w.Code, w.Body.String())
		}
		var resp struct {
			Pending []runtime.PendingAwait `json:"pending"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(resp.Pending) != 1 || resp.Pending[0].AwaitName != wantName {
			t.Fatalf("pending %s = %+v, want exactly one await named %q", path, resp.Pending, wantName)
		}
	}

	// Antenna leg: only the on-air program's await, never the preview one.
	assertOnlyAwait(t, "/api/v1/runtime/_/pending", "pick-onair")
	// Preview leg: only the preview program's await, never the on-air one.
	assertOnlyAwait(t, "/api/v1/runtime/_/pending?target=preview", "pick-preview")
}

// TestCockpit_AntennaAwaitsIsolatedFromPopulatedPreviewSlot proves GET
// /cockpit/contracts's antenna leg isolation under the same "both slots
// populated with distinct awaits" pressure. Unlike getRuntimePending,
// getCockpitContracts's Engine B leg (appendEngineBScene, cockpit.go) does
// NOT route through engineBSlot(r) — its only call site hardcodes
// bluehost.SlotOnAir (cockpit.go ~line 126); ?target=preview instead reads
// Engine A (operatorTarget) and never touches bluehost.Host at all. So the
// isolation guarantee here rests structurally on that hardcoded argument,
// not on engineBSlot's branch — this test pins that fact: even with a
// DIFFERENTLY NAMED await armed on SlotPreview at the same time, the
// antenna's cockpit contract must carry only the on-air one.
func TestCockpit_AntennaAwaitsIsolatedFromPopulatedPreviewSlot(t *testing.T) {
	onAirProgram := buildEngineBOperatorProgram(t, "call-onair", "called-onair",
		"pick-onair", "core.primitive.integer", "picked-onair")
	previewProgram := buildEngineBOperatorProgram(t, "call-preview", "called-preview",
		"pick-preview", "core.primitive.integer", "picked-preview")

	host := bluehost.NewHost()
	if err := host.Take("dual-onair", "sha256:dual-onair", onAirProgram, nil, nil, nil); err != nil {
		t.Fatalf("Take on-air: %v", err)
	}
	if _, err := host.Step(bluehost.SlotOnAir); err != nil {
		t.Fatalf("Step on-air (on-start): %v", err)
	}
	if err := host.Prepare(bluehost.SlotPreview, "dual-preview", "dual-preview-scene",
		"sha256:dual-preview", previewProgram, nil, nil, nil); err != nil {
		t.Fatalf("Prepare preview: %v", err)
	}
	if _, err := host.Step(bluehost.SlotPreview); err != nil {
		t.Fatalf("Step preview (on-start): %v", err)
	}
	// Sanity: both slots really are armed with distinct names before the
	// HTTP assertion — a false pass here (e.g. an empty list) would make the
	// isolation check below vacuous.
	if names := host.PendingAwaitNames(bluehost.SlotOnAir); len(names) != 1 || names[0] != "pick-onair" {
		t.Fatalf("on-air not armed as expected: %#v", names)
	}
	if names := host.PendingAwaitNames(bluehost.SlotPreview); len(names) != 1 || names[0] != "pick-preview" {
		t.Fatalf("preview not armed as expected: %#v", names)
	}

	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	t.Cleanup(show.Stop)
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger: testLogger(), Metrics: m, Show: show,
		SceneIntent: &SceneIntentDeps{Host: host},
	})
	f := &cockpitFixture{mux: mux}

	_, body := getContracts(t, f, "operator", "?stream_id=s1")
	if len(body.Awaits) != 1 {
		t.Fatalf("awaits = %+v, want exactly one (on-air only, preview must not leak)", body.Awaits)
	}
	if body.Awaits[0].AwaitName != "pick-onair" {
		t.Fatalf("awaits[0].await_name = %q, want pick-onair (preview leaked into antenna contract)",
			body.Awaits[0].AwaitName)
	}
}
