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
func TestSceneIntentMirrorFor_RoutesBySlot(t *testing.T) {
	preview := &recordingLsdpRegistry{}
	antenne := &recordingLsdpRegistry{}
	mirrorFor := sceneIntentMirrorFor(preview, antenne)

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
