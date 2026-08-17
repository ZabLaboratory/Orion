package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// ORION-OPERATOR-PREVIEW-ENGINE-B: ?target=preview now routes call/resolve/
// pending through bluehost.Host's SlotPreview (populated by
// POST /host/scene-intent's prepare-preview action) instead of the dead
// Engine A PreviewSlot (operator.go's file-header doc). These tests prove
// the three transitions the migration exists for — fail-closed-but-true on
// an empty preview slot, success on a populated one, ENTRYPOINT_UNKNOWN/
// AWAIT_GONE become reachable — plus antenna non-regression and slot
// isolation both ways.

// --- call, ?target=preview -------------------------------------------------

func TestOperator_CallPreviewFiresWhenSlotPreviewHosted(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "call", "called", "", "", "")
	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotPreview, program)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call?target=preview", "operator",
		map[string]any{"payload": "preview-fired"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("preview call: got %d, want 202 (body=%s)", w.Code, w.Body.String())
	}
	if got, _ := f.peekVarSlot(t, bluehost.SlotPreview, "called").(string); got != "preview-fired" {
		t.Fatalf("called = %#v, want %q", got, "preview-fired")
	}
}

func TestOperator_CallPreviewEmptySlotIs409(t *testing.T) {
	// No SceneIntent/Host at all — the exact symptom the bail reports:
	// POST .../operator/call/_/LEC?target=preview against an unprovisioned
	// preview slot.
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call?target=preview", "operator",
		map[string]any{"payload": 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("empty preview slot: got %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "BLUEPRINT_NOT_ACTIVE" {
		t.Fatalf("error = %q, want BLUEPRINT_NOT_ACTIVE", resp.Error)
	}
}

func TestOperator_CallPreviewUnknownEntrypointIs409(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "call", "called", "", "", "")
	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotPreview, program)
	// Unlike the empty-slot case, the slot IS hosted — this must now reach
	// the HasTrigger pre-check and come back ENTRYPOINT_UNKNOWN, not
	// BLUEPRINT_NOT_ACTIVE. Before the migration this code was never
	// reachable via ?target=preview at all (the slot was always empty).
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/nope?target=preview", "operator",
		map[string]any{"payload": 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("unknown preview entrypoint: got %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "ENTRYPOINT_UNKNOWN" {
		t.Fatalf("error = %q, want ENTRYPOINT_UNKNOWN", resp.Error)
	}
}

// --- call, antenna non-regression + slot isolation -------------------------

func TestOperator_CallAntennaUnaffectedByPreviewMigration(t *testing.T) {
	// Absent ?target still resolves to SlotOnAir, unchanged — the plain
	// TestOperator_CallFiresWithPayload/RealBlueprintID cases already prove
	// this positively; this one proves the negative: a program ONLY in
	// SlotPreview must not leak onto a plain antenna call.
	program := buildEngineBOperatorProgram(t, "call", "called", "", "", "")
	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotPreview, program)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call", "operator",
		map[string]any{"payload": 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("antenna call against preview-only program: got %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
}

func TestOperator_CallSlotIsolationBothWays(t *testing.T) {
	onAirProgram := buildEngineBOperatorProgram(t, "call-antenna", "called-antenna", "", "", "")
	previewProgram := buildEngineBOperatorProgram(t, "call-preview", "called-preview", "", "", "")
	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotOnAir, onAirProgram)
	f.takeSlot(t, bluehost.SlotPreview, previewProgram)

	// The antenna's entrypoint is unreachable through ?target=preview...
	wLeakToPreview := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call-antenna?target=preview",
		"operator", map[string]any{"payload": 1})
	if wLeakToPreview.Code != http.StatusConflict {
		t.Fatalf("antenna entrypoint via preview: got %d, want 409 (body=%s)",
			wLeakToPreview.Code, wLeakToPreview.Body.String())
	}
	// ...and the preview's entrypoint is unreachable through the antenna —
	// the exact invariant the bail requires: preparing a scene in preview
	// must never tirer sur l'antenne.
	wLeakToAntenna := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call-preview",
		"operator", map[string]any{"payload": 1})
	if wLeakToAntenna.Code != http.StatusConflict {
		t.Fatalf("preview entrypoint via antenna: got %d, want 409 (body=%s)",
			wLeakToAntenna.Code, wLeakToAntenna.Body.String())
	}

	// Each fires correctly on its OWN slot, and only there.
	wAntenna := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call-antenna",
		"operator", map[string]any{"payload": "on-air"})
	if wAntenna.Code != http.StatusAccepted {
		t.Fatalf("antenna call: got %d, want 202 (body=%s)", wAntenna.Code, wAntenna.Body.String())
	}
	wPreview := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call-preview?target=preview",
		"operator", map[string]any{"payload": "preview"})
	if wPreview.Code != http.StatusAccepted {
		t.Fatalf("preview call: got %d, want 202 (body=%s)", wPreview.Code, wPreview.Body.String())
	}
	if got, _ := f.peekVarSlot(t, bluehost.SlotOnAir, "called-antenna").(string); got != "on-air" {
		t.Fatalf("called-antenna = %#v, want %q", got, "on-air")
	}
	if got, _ := f.peekVarSlot(t, bluehost.SlotPreview, "called-preview").(string); got != "preview" {
		t.Fatalf("called-preview = %#v, want %q", got, "preview")
	}
}

// --- resolve, ?target=preview -----------------------------------------------

func TestOperator_ResolvePreviewResumesWhenSlotPreviewHosted(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "call", "called", "pick", "core.primitive.integer", "picked")
	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotPreview, program)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/_/pick?target=preview", "operator",
		map[string]any{"value": 7})
	if w.Code != http.StatusOK {
		t.Fatalf("preview resolve: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if got, _ := f.peekVarSlot(t, bluehost.SlotPreview, "picked").(float64); got != 7 {
		t.Fatalf("picked = %#v, want 7", got)
	}
}

func TestOperator_ResolvePreviewEmptySlotIs410(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/_/pick?target=preview", "operator",
		map[string]any{"value": 1})
	if w.Code != http.StatusGone {
		t.Fatalf("empty preview slot resolve: got %d, want 410 (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != "AWAIT_GONE" {
		t.Fatalf("error = %q, want AWAIT_GONE", resp.Error)
	}
}

func TestOperator_ResolvePreviewTypeMismatchIs422(t *testing.T) {
	// Hosted-but-wrong-value proves the type-check path is reachable through
	// ?target=preview too, not just BLUEPRINT_NOT_ACTIVE/AWAIT_GONE.
	program := buildEngineBOperatorProgram(t, "call", "called", "pick", "core.primitive.integer", "picked")
	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotPreview, program)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/_/pick?target=preview", "operator",
		map[string]any{"value": "not-an-integer"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preview resolve type mismatch: got %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
}

// --- resolve, antenna non-regression + rule non-regression -----------------

func TestOperator_ResolveAntennaUnaffectedByPreviewMigration(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "call", "called", "pick", "core.primitive.integer", "picked")
	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotPreview, program)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/_/pick", "operator",
		map[string]any{"value": 1})
	if w.Code != http.StatusGone {
		t.Fatalf("antenna resolve against preview-only await: got %d, want 410 (body=%s)", w.Code, w.Body.String())
	}
}

// --- pending: Engine B leg is honestly, deliberately always empty ----------

// TestOperator_PendingEngineBAlwaysEmptyByDesign pins operator.go's "ENGINE
// B PENDING GAP": blueruntime exposes no live-armed-awaits accessor
// (bluehost.AwaitDecl is the DECLARED set only), so pending's Engine B leg
// cannot honestly report a populated list — not for the antenna, not for
// preview — whether or not the slot is hosted. This is not a regression
// versus the old behaviour (also always empty in production, since
// Show.Active() has had no populator since #331): it is the same observable
// result reached through the correct selector instead of a dead one, with
// the gap now documented instead of accidental.
func TestOperator_PendingEngineBAlwaysEmptyByDesign(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "call", "called", "pick", "core.primitive.integer", "picked")

	cases := []struct {
		name string
		path string
	}{
		{"antenna, hosted", "/api/v1/runtime/_/pending"},
		{"preview, hosted", "/api/v1/runtime/_/pending?target=preview"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotOnAir, program)
			f.takeSlot(t, bluehost.SlotPreview, program)
			w := opRequest(t, f.mux, "GET", c.path, "operator", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("pending: got %d, want 200 (body=%s)", w.Code, w.Body.String())
			}
			var resp struct {
				Pending []runtime.PendingAwait `json:"pending"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Pending) != 0 {
				t.Fatalf("pending = %#v, want empty (Engine B has no live-armed accessor)", resp.Pending)
			}
		})
	}
}

// TestOperator_PendingEngineBEmptyWhenUnhosted proves the unhosted case
// answers identically (200, empty) — no panic, no error, no engine-nil
// crash — matching the dormant convention documented at the file header
// ("empty (pending)").
func TestOperator_PendingEngineBEmptyWhenUnhosted(t *testing.T) {
	f := newOperatorFixture(t) // no SceneIntent/Host at all
	for _, target := range []string{"", "preview"} {
		path := "/api/v1/runtime/_/pending"
		if target != "" {
			path += "?target=" + target
		}
		w := opRequest(t, f.mux, "GET", path, "operator", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("pending (target=%q): got %d, want 200 (body=%s)", target, w.Code, w.Body.String())
		}
	}
}
