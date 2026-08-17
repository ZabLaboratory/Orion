package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// TestOperator_UnknownTargetRejected (ORION-UNKNOWN-TARGET-CONTRACT,
// Prism#740) replaces the former TestOperator_UnknownTargetFailsOpenToAntenna
// (PR#391): a `?target=` value that is neither absent nor the literal
// "preview" used to silently fail open to bluehost.SlotOnAir — the live
// antenna — same as no selector at all. That test only pinned the behaviour
// ("does not endorse", per its own comment) and named hardening it into a
// 400 as the decision this work unit makes. It now does: resolveTargetKind
// (operator.go) rejects it with 400 UNKNOWN_TARGET before either engine is
// touched, on every route that reads ?target=.
//
// Both slots are populated with DISTINGUISHABLE programs (different state
// variable names) so a regression that routed the rejected call to EITHER
// engine — not just a fail-open to the antenna — is caught: nothing may
// fire, anywhere.
func TestOperator_UnknownTargetRejected(t *testing.T) {
	onAirProgram := buildEngineBOperatorProgram(t, "call", "called-onair", "", "", "")
	previewProgram := buildEngineBOperatorProgram(t, "call", "called-preview", "", "", "")
	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotOnAir, onAirProgram)
	f.takeSlot(t, bluehost.SlotPreview, previewProgram)

	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call?target=bogus", "operator",
		map[string]any{"payload": "typo-target"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown target call: got %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	var errBody map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v (raw=%s)", err, w.Body.String())
	}
	if errBody["error"] != "UNKNOWN_TARGET" {
		t.Fatalf("error code = %q, want UNKNOWN_TARGET (body=%s)", errBody["error"], w.Body.String())
	}

	if got := f.peekVarSlot(t, bluehost.SlotOnAir, "called-onair"); got != nil {
		t.Fatalf("antenna var called-onair = %#v, want nil — a rejected target must not fire on the antenna", got)
	}
	if got := f.peekVarSlot(t, bluehost.SlotPreview, "called-preview"); got != nil {
		t.Fatalf("preview var called-preview = %#v, want nil — a rejected target must not leak to preview either", got)
	}
}

// TestOperator_UnknownTargetRejected_ResolveAndPending prove the same
// decision point (resolveTargetKind) guards the other two Engine-B-routed
// legs, not just call — a route that forgot to call it would silently
// diverge, which is exactly the failure mode unifying the check exists to
// close (see resolveTargetKind's doc).
func TestOperator_UnknownTargetRejected_ResolveAndPending(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "call", "called", "pick", "core.primitive.integer", "picked")
	f := newEngineBOperatorFixture(t, program)

	wResolve := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/_/pick?target=bogus", "operator",
		map[string]any{"value": 1})
	if wResolve.Code != http.StatusBadRequest {
		t.Fatalf("unknown target resolve: got %d, want 400 (body=%s)", wResolve.Code, wResolve.Body.String())
	}

	wPending := opRequest(t, f.mux, "GET", "/api/v1/runtime/_/pending?target=bogus", "operator", nil)
	if wPending.Code != http.StatusBadRequest {
		t.Fatalf("unknown target pending: got %d, want 400 (body=%s)", wPending.Code, wPending.Body.String())
	}
}
