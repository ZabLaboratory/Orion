package api

import (
	"net/http"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// TestOperator_UnknownTargetFailsOpenToAntenna (ORION-OPERATOR-TEST-
// HARDENING, Prism#740, gap 3) pins engineBSlot's existing fail-open: a
// `?target=` value that is neither absent nor the literal "preview" resolves
// to bluehost.SlotOnAir — the live antenna — same as no selector at all.
// This is PRE-EXISTING behaviour, unchanged by the preview-Engine-B
// migration (55062a1f) or the mode gate (1a35f9f4), but newly material now
// that preview is a real, populated slot: a typo in a client's query string
// (?target=preveiw, ?target=Preview, any other value) silently fires on the
// antenna instead of erroring or landing on preview.
//
// This test does NOT endorse the behaviour — hardening it into a 400
// UNKNOWN_TARGET is a contract decision that belongs elsewhere (team-lead's
// bail explicitly routes it away from this chantier). It only epingles
// today's contract so a future silent change here is forced to make this
// test go red on purpose, not by accident.
//
// Both slots are populated with DISTINGUISHABLE programs (different state
// variable names) so a fail-open bug that instead routed to preview, or that
// dropped the call, is caught by the negative assertion on SlotPreview.
func TestOperator_UnknownTargetFailsOpenToAntenna(t *testing.T) {
	onAirProgram := buildEngineBOperatorProgram(t, "call", "called-onair", "", "", "")
	previewProgram := buildEngineBOperatorProgram(t, "call", "called-preview", "", "", "")
	f := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotOnAir, onAirProgram)
	f.takeSlot(t, bluehost.SlotPreview, previewProgram)

	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call?target=bogus", "operator",
		map[string]any{"payload": "typo-target"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("unknown target call: got %d, want 202 (body=%s)", w.Code, w.Body.String())
	}

	if got, _ := f.peekVarSlot(t, bluehost.SlotOnAir, "called-onair").(string); got != "typo-target" {
		t.Fatalf("antenna var called-onair = %#v, want %q — unknown target did not fail open to the antenna (fail-open contract broken, or route changed)",
			f.peekVarSlot(t, bluehost.SlotOnAir, "called-onair"), "typo-target")
	}
	if got := f.peekVarSlot(t, bluehost.SlotPreview, "called-preview"); got != nil {
		t.Fatalf("preview var called-preview = %#v, want nil — unknown target leaked into preview instead of the antenna", got)
	}
}
