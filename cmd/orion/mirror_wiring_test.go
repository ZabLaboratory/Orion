package main

import (
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// recordingLsdpRegistry is a bare lsdpMirrorRegistry stand-in that
// records every MirrorForLSML call it receives — no real LSDP websocket
// kit needed to prove routing.
type recordingLsdpRegistry struct {
	calls int
}

func (r *recordingLsdpRegistry) MirrorForLSML(string, string, []byte) runtime.SceneMirror {
	r.calls++
	return nil
}

// TestSceneIntentMirrorFor_RoutesBySlot is the #398 resolution criterion
// at its narrowest, most direct point: a prepare-preview (SlotPreview)
// must produce zero calls on the antenne registry — the test the issue
// demands must fail on the pre-fix code, where MirrorFor had no slot
// parameter at all and every call landed on the antenne wire regardless.
// The symmetric take case (SlotOnAir → antenne, zero preview calls) is
// proven in the same test, since a one-sided assertion is worthless
// (#398's own point dur: "un test qui prouve que le preview ne touche
// plus l'antenne mais qui ne prouve pas que le take l'atteint toujours
// ne vaut rien").
//
// The lsdpWires{preview: ..., antenne: ...} literal below is the EXACT
// same construction pattern main.go's real call site writes (F1, Vigil
// review on d448def) — named fields, not two positional same-typed
// arguments. Before this fix, a test built its own
// sceneIntentMirrorFor(a, b) call and could never observe a silent
// argument-order swap at the ACTUAL call site in main.go, since the test
// always wrote its own arguments "in the right order" by construction.
// With named fields there is no order left to swap: main.go and this
// test both write `lsdpWires{preview:, antenne:}`, so a routing bug can
// only come from writing the wrong FIELD NAME — a visible, deliberate
// edit that stands out in review, not an invisible transposition a
// diff-blind refactor could introduce.
func TestSceneIntentMirrorFor_RoutesBySlot(t *testing.T) {
	preview := &recordingLsdpRegistry{}
	antenne := &recordingLsdpRegistry{}
	mirrorFor := sceneIntentMirrorFor(lsdpWires{preview: preview, antenne: antenne})

	mirrorFor("scene-1", "sha256:test-1", bluehost.SlotPreview, nil)
	if preview.calls != 1 {
		t.Fatalf("prepare-preview must call the preview wire exactly once, got %d", preview.calls)
	}
	if antenne.calls != 0 {
		t.Fatalf("prepare-preview must produce ZERO calls on the antenne wire — got %d (#398 defect)", antenne.calls)
	}

	mirrorFor("scene-1", "sha256:test-1", bluehost.SlotOnAir, nil)
	if antenne.calls != 1 {
		t.Fatalf("take must call the antenne wire exactly once, got %d", antenne.calls)
	}
	if preview.calls != 1 {
		t.Fatalf("take must NOT touch the preview wire — preview calls changed to %d", preview.calls)
	}
}

// TestSceneIntentMirrorFor_UnknownSlotFailsClosed is the F2 fix (Vigil
// review on d448def): bluehost.Slot is `type Slot string`, not a closed
// enum, so a slot value that is neither SlotPreview nor SlotOnAir must
// fail CLOSED (nil mirror, no bridge) rather than silently fall through
// to the live antenne wire — the exact "unknown → live" shape #398
// exists to remove. No path reaches this branch today (slot is derived
// strictly binary from the attested action in startBridge); this pins
// the posture so a future third slot value cannot regress into it.
func TestSceneIntentMirrorFor_UnknownSlotFailsClosed(t *testing.T) {
	preview := &recordingLsdpRegistry{}
	antenne := &recordingLsdpRegistry{}
	mirrorFor := sceneIntentMirrorFor(lsdpWires{preview: preview, antenne: antenne})

	got := mirrorFor("scene-1", "sha256:test-1", bluehost.Slot("unknown"), nil)

	if got != nil {
		t.Fatalf("an unrecognised slot must return a nil mirror, got %v", got)
	}
	if preview.calls != 0 || antenne.calls != 0 {
		t.Fatalf("an unrecognised slot must call NEITHER wire — preview=%d antenne=%d", preview.calls, antenne.calls)
	}
}
