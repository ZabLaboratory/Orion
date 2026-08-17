package bluehost_test

import (
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/providers"
)

// TestHost_PreviewNeverReceivesCompletionEvenWithoutHTTPEffects (ORION-
// OPERATOR-TEST-HARDENING, Prism#740, gap 2) falsifies the GATE'S POSITION
// inside dispatchInvocations, not merely its presence.
// TestHost_PreviewHTTPEffectGateBlocksRealDispatch already proves a WIRED
// (egress+runner set) preview instance never dials — but with dependencies
// wired, the `if modeFor(slot) != blueruntime.Execute` early-return, WHEREVER
// placed relative to the `egress == nil || runner == nil` fallback block,
// still runs before the dispatch loop that would submit a real job. Moving
// the gate below that fallback block only bites in the ABSENT-dependencies
// case: dispatchInvocations' fallback loop completes every invocation with
// EFFECT_PROVIDER_UNAVAILABLE unconditionally — if the mode gate sits below
// it, a preview instance whose HTTP effects were never wired (SetHTTPEffects
// never called) receives that injected completion instead of the correct
// behaviour, total silence.
//
// This test never calls SetHTTPEffects — h's httpEgress/httpRunner stay
// nil — and asserts the preview instance's on-event completion sink NEVER
// fires, not "succeeded", not "failed", nothing at all. It reuses
// customHTTPProviderWithoutPreviewNoop (effect_http_integration_test.go) so
// blueruntime's own walker-level preview-noop short-circuit does not mask
// the Host-level gate this test targets — same isolation rationale as
// TestHost_PreviewHTTPEffectGateBlocksRealDispatch.
//
// Verified by hand: moving the `if modeFor(slot) != blueruntime.Execute`
// early-return in dispatchInvocations (effect_http.go) to AFTER the
// `if egress == nil || runner == nil { ... return }` fallback block turns
// this red (a "failed"/EFFECT_PROVIDER_UNAVAILABLE completion lands in
// step.Variables["result"]); restoring the gate as the function's very
// first instruction — before even reading h.httpEgress/h.httpRunner — turns
// it green. The CI diff itself never contains that mutation.
func TestHost_PreviewNeverReceivesCompletionEvenWithoutHTTPEffects(t *testing.T) {
	h := bluehost.NewHost()
	// Deliberately no SetHTTPEffects call: h's httpEgress/httpRunner stay
	// nil, exercising the EFFECT_PROVIDER_UNAVAILABLE fallback branch the
	// gate must dominate regardless of where it sits relative to it.

	program := buildHTTPEffectProgram(t, "https://unreachable.invalid/effect")
	if err := h.Prepare(bluehost.SlotPreview, "preview-no-http-effects", "preview-no-http-effects-scene",
		"sha256:preview-no-http-effects", program, customHTTPProviderWithoutPreviewNoop(), providers.Policy(true), nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	step, err := h.Step(bluehost.SlotPreview)
	if err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	if len(step.Invocations) != 1 {
		t.Fatalf("on-start invocations = %d, want 1 (walker-level short-circuit engaged — this test is not exercising the Host gate)", len(step.Invocations))
	}

	// A real completion (of any status) lands well under 500ms on every other
	// test in this file. 500ms of silence is conclusive that dispatchInvocations
	// returned before completing anything.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		step, err := h.Step(bluehost.SlotPreview)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if value, ok := step.Variables["result"].(map[string]any); ok {
			t.Fatalf("preview instance received a completion with no HTTP effects wired (gate positioned after the EFFECT_PROVIDER_UNAVAILABLE fallback): %#v", value)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
