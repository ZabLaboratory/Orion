package bluehost

import (
	"os"
	"testing"
)

// fakeOverlayMirror records every EmitOverlayApp call verbatim — used to
// assert dispatchOverlayAppSet's OWN dispatch/dedup/mode-gate logic in
// isolation, fast and deterministic. internal/lsdp/overlay_streamrule_test.go
// proves the same wiring end to end against the REAL lsdp.Wire and a real
// WS client (the "reached the wire", not just "called our fake" bar).
type fakeOverlayMirror struct {
	calls []overlayMirrorCall
}

type overlayMirrorCall struct {
	appID          string
	running, onAir *bool
}

func (m *fakeOverlayMirror) EmitOverlayApp(appID string, running, onAir *bool) {
	m.calls = append(m.calls, overlayMirrorCall{appID: appID, running: running, onAir: onAir})
}

// streamRuleOverlayFixture reads the hand-composed blue.program.v1 stream-rule
// fixture: on-start -> core.overlay-app.set@1, no scene-carrying opcode
// anywhere in it. Shared with internal/lsdp's end-to-end proof.
func streamRuleOverlayFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../bluespike/testdata/03-overlay-app-stream-rule.program.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// TestDispatchOverlayAppSet_OnAirReachesMirror proves core.overlay-app.set@1
// firing on-air is forwarded to the configured OverlayAppMirror with the
// authored app_id/running/on_air — end to end through Host.Take + Host.Step,
// the exact call path a real stream-rule uses (ORION-OVERLAY-EFFECTOR-
// STREAM-RULE-PROOF).
func TestDispatchOverlayAppSet_OnAirReachesMirror(t *testing.T) {
	h := NewHost()
	mirror := &fakeOverlayMirror{}
	h.SetOverlayMirror(mirror)

	if err := h.Take("stream-rule-1", "sha256:stream-rule", streamRuleOverlayFixture(t), nil, nil, nil); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if _, err := h.Step(SlotOnAir); err != nil { // fires on-start
		t.Fatalf("Step (on-start): %v", err)
	}

	if len(mirror.calls) != 1 {
		t.Fatalf("EmitOverlayApp calls = %d, want 1 (got %#v)", len(mirror.calls), mirror.calls)
	}
	call := mirror.calls[0]
	if call.appID != "stream-rule-overlay" {
		t.Fatalf("app_id = %q, want %q", call.appID, "stream-rule-overlay")
	}
	if call.running == nil || !*call.running {
		t.Fatalf("running = %v, want true", call.running)
	}
	if call.onAir == nil || !*call.onAir {
		t.Fatalf("on_air = %v, want true", call.onAir)
	}
}

// TestDispatchOverlayAppSet_UnchangedRecordNotReDispatched proves the
// edge-detector: StepResult.Variables carries the FULL cumulative
// ctx.variables bag on every call (runtime.go's cloneMap(instance.variables)),
// so a second Step with nothing new must NOT re-call EmitOverlayApp for a
// record already dispatched — lumencast-go's SetOverlayApps has no dedup of
// its own (server.go) and would otherwise re-broadcast to every subscriber
// on every Tick forever.
func TestDispatchOverlayAppSet_UnchangedRecordNotReDispatched(t *testing.T) {
	h := NewHost()
	mirror := &fakeOverlayMirror{}
	h.SetOverlayMirror(mirror)

	if err := h.Take("stream-rule-1", "sha256:stream-rule", streamRuleOverlayFixture(t), nil, nil, nil); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if _, err := h.Step(SlotOnAir); err != nil {
		t.Fatalf("Step 1 (on-start): %v", err)
	}
	if len(mirror.calls) != 1 {
		t.Fatalf("after step 1: EmitOverlayApp calls = %d, want 1", len(mirror.calls))
	}

	// The instance is now idle (no inbox, no further entrypoint armed) —
	// Step is a legal no-op transition, same as a periodic Tick finding
	// nothing due. The overlay-app.set node did not fire again, but its
	// record from step 1 is still sitting in the cumulative bag.
	if _, err := h.Step(SlotOnAir); err != nil {
		t.Fatalf("Step 2 (idle): %v", err)
	}
	if len(mirror.calls) != 1 {
		t.Fatalf("after step 2 (nothing new fired): EmitOverlayApp calls = %d, want still 1 (got %#v)", len(mirror.calls), mirror.calls)
	}
}

// TestDispatchOverlayAppSet_PreviewNeverReachesMirror proves the preview
// policy this file documents: blueruntime.Preview never touches the real
// antenna wire, the same posture NewEffectHandlers already applies to
// http/db/service.call (effects.go). The bag write still happens
// (walker.go, unconditional); only the forward to the real mirror is
// suppressed.
func TestDispatchOverlayAppSet_PreviewNeverReachesMirror(t *testing.T) {
	h := NewHost()
	mirror := &fakeOverlayMirror{}
	h.SetOverlayMirror(mirror)

	if err := h.Prepare(SlotPreview, "preview-1", "scene-stream-rule", "sha256:stream-rule", streamRuleOverlayFixture(t), nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	step, err := h.Step(SlotPreview) // fires on-start
	if err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}

	if len(mirror.calls) != 0 {
		t.Fatalf("preview must never reach the real mirror, got %#v", mirror.calls)
	}
	bag, _ := step.Variables[overlayAppSetBag].(map[string]any)
	if len(bag) != 1 {
		t.Fatalf("preview overlay-app.set must still land in the reserved bag, got %#v", step.Variables)
	}
}

// TestOverlayAppSetRecordFields_ConfigFallback proves the inputs->config
// precedence: a record whose app_id/running/on_air only exist under
// `config` (the shape a hand-built low-level fixture that bakes values
// straight onto node.config produces — this package's own fixture, and
// internal/runtime's A/B parity harness for this exact opcode, both take
// this shape) still resolves correctly.
func TestOverlayAppSetRecordFields_ConfigFallback(t *testing.T) {
	record := map[string]any{
		"config": map[string]any{"app_id": "cfg-app", "running": true},
		"inputs": map[string]any{},
	}
	appID, running, onAir, ok := overlayAppSetRecordFields(record)
	if !ok || appID != "cfg-app" {
		t.Fatalf("appID=%q ok=%v, want cfg-app/true", appID, ok)
	}
	if running == nil || !*running {
		t.Fatalf("running = %v, want true", running)
	}
	if onAir != nil {
		t.Fatalf("on_air = %v, want nil (never set)", *onAir)
	}
}

// TestOverlayAppSetRecordFields_InputsTakePrecedence proves a wired data
// input overrides a stale/default config literal for the same port.
func TestOverlayAppSetRecordFields_InputsTakePrecedence(t *testing.T) {
	record := map[string]any{
		"config": map[string]any{"app_id": "stale", "running": false},
		"inputs": map[string]any{"app_id": "fresh", "running": true},
	}
	appID, running, _, ok := overlayAppSetRecordFields(record)
	if !ok || appID != "fresh" {
		t.Fatalf("appID=%q ok=%v, want fresh/true", appID, ok)
	}
	if running == nil || !*running {
		t.Fatalf("running = %v, want true (from inputs, not stale config)", running)
	}
}

// TestOverlayAppSetRecordFields_MissingAppIDSkipped mirrors Engine A's
// execOverlayAppSet: app_id absent skips the emission (never a crash).
func TestOverlayAppSetRecordFields_MissingAppIDSkipped(t *testing.T) {
	if _, _, _, ok := overlayAppSetRecordFields(map[string]any{"config": map[string]any{"running": true}}); ok {
		t.Fatal("expected ok=false without app_id")
	}
	if _, _, _, ok := overlayAppSetRecordFields(nil); ok {
		t.Fatal("expected ok=false for a nil record")
	}
}

// TestOverlayAppSetRecordFields_BothDimensionsAbsentSkipped mirrors
// EmitOverlayApp's own no-op rule (overlay_mirror.go: "running == nil &&
// onAir == nil" is dropped) — skip before ever reaching the mirror.
func TestOverlayAppSetRecordFields_BothDimensionsAbsentSkipped(t *testing.T) {
	if _, _, _, ok := overlayAppSetRecordFields(map[string]any{"config": map[string]any{"app_id": "a"}}); ok {
		t.Fatal("expected ok=false when both running and on_air are absent")
	}
}
