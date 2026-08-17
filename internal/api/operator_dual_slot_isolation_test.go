package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Dual-slot poll isolation (Prism#740, Bastion clearance finding on #392).
//
// #392's join (engineBArmedAwaits, operator.go) is the first thing that lets
// GET /runtime/{blueprint_id}/pending serve a POPULATED awaits facet at all.
// Every existing fixture that already exercises both slots together
// (TestOperator_CallSlotIsolationBothWays) leaves the OTHER slot's AWAITS
// empty — its program either declares none, or declares one that on-start
// never arms. A routing bug that swapped, duplicated, or hardcoded the slot
// argument on the read path would still answer back an empty list on that
// leg, which reads identically to "this slot legitimately has nothing
// armed" — the exact ambiguity the bail calls out. This test instead arms a
// DIFFERENTLY NAMED await on BOTH slots at the same time, so a
// swapped/duplicated slot surfaces a name the polled leg must never produce,
// not just a non-empty list.
//
// SCOPE NOTE: GET /cockpit/contracts is deliberately NOT covered here.
// Correction from the bail owner: engineBSlot is NOT the single decision
// point for that route — cockpit.go:117-122 picks its slot with a separate
// literal `target == "preview"` check that calls neither isEngineBRouted nor
// engineBSlot, and the comment on engineBSlot itself carries that same
// false "unique decision point" claim. #392's join only reached the antenna
// leg of /cockpit/contracts; ?target=preview there still reads Engine A
// (operatorTarget/deps.Preview). A parallel unit owns cockpit.go's slot
// decision and its tests — this file stays clear of it to avoid collision.

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
