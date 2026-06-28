package lsdp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	lproto "github.com/Lumencast/lumencast-go/protocol"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// TestLSDP_PreviewSwitchDoesNotTouchAntenne is the working-model proof of the
// preview/antenne split. A live show is active on scene-1 with an LSDP
// subscriber attached (= the antenne), and a PERSISTENT preview wire carries a
// preview clone via PreviewSlot. SWITCHING the previewed scene (Activate
// scene-1 → scene-2) MUST migrate the preview subscriber (a scene-2 keyframe,
// the proven SetActive path) and MUST NOT emit any frame on the antenne's
// /show/stream.lsdp wire — i.e. switching a scene in preview no longer flips
// the live antenne. The shared scene id (both start on scene-1) proves the two
// wires are independent kit Servers with no cross-scene collision.
func TestLSDP_PreviewSwitchDoesNotTouchAntenne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- Antenne: global show + LSDP wire, active on scene-1. ---
	_, globalScene, globalWsURL := dualShow(t)

	// --- Preview: a SECOND persistent wire + a PreviewSlot over it. ---
	logger := quietLogger(t)
	previewWire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire preview: %v", err)
	}
	slot := runtime.NewPreviewSlot(ctx, runtime.NewComputeRegistry(), previewWire, logger)
	t.Cleanup(slot.Close)
	previewWsURL := mountWire(t, previewWire)

	// Preview starts on scene-1 (same id as the antenne).
	slot.Activate("scene-1",
		passthroughGraph("scene-1", "sha256:test-1", "score.team_a"),
		&compiler.RenderBundle{SceneVersion: "sha256:test-1"})

	// Antenne subscriber attaches, drains its keyframe (scene-1 @ 0).
	antenne := dialLSDP(ctx, t, globalWsURL, "viewer", 0)
	defer antenne.Close(websocket.StatusNormalClosure, "")
	if snap, ok := readServerFrame(ctx, t, antenne).(*lproto.Snapshot); !ok {
		t.Fatalf("antenne first frame must be a snapshot")
	} else if snap.SceneID != "scene-1" {
		t.Fatalf("antenne keyframe scene = %q, want scene-1", snap.SceneID)
	}

	// Preview subscriber attaches, drains its keyframe (scene-1).
	preview := dialLSDP(ctx, t, previewWsURL, "operator", 0)
	defer preview.Close(websocket.StatusNormalClosure, "")
	if snap, ok := readServerFrame(ctx, t, preview).(*lproto.Snapshot); !ok {
		t.Fatalf("preview first frame must be a snapshot")
	} else if snap.SceneID != "scene-1" {
		t.Fatalf("preview keyframe scene = %q, want scene-1", snap.SceneID)
	}

	// --- THE SWITCH: preview moves to scene-2 (the regression vector). ---
	slot.Activate("scene-2",
		passthroughGraph("scene-2", "sha256:test-2", "board.row0"),
		&compiler.RenderBundle{SceneVersion: "sha256:test-2"})

	// Preview subscriber migrates to scene-2 over its EXISTING socket: a
	// SceneChanged then (LSDP/1.1 §3.3.1) a Snapshot — no reconnect, the proven
	// antenne switch path applied to the preview wire.
	if sc, ok := readServerFrame(ctx, t, preview).(*lproto.SceneChanged); !ok {
		t.Fatalf("preview switch: first frame must be scene_changed")
	} else if sc.SceneID != "scene-2" {
		t.Fatalf("preview scene_changed = %q, want scene-2", sc.SceneID)
	}
	if snap, ok := readServerFrame(ctx, t, preview).(*lproto.Snapshot); !ok {
		t.Fatalf("preview switch: scene_changed must be followed by a snapshot")
	} else if snap.SceneID != "scene-2" {
		t.Fatalf("preview switch snapshot scene = %q, want scene-2", snap.SceneID)
	}

	// Reverse direction: a change on the global scene reaches the antenne with
	// its OWN value. If the preview switch had leaked onto the antenne wire,
	// the antenne's FIRST queued frame here would be the scene-2 snapshot (a
	// Snapshot, not a Delta) — so this assertion also catches the leak.
	if !globalScene.Input(runtime.InputMsg{
		Path:  "score.team_a",
		Value: json.RawMessage(`21`),
	}) {
		t.Fatal("global scene inbox full")
	}
	if d, ok := readServerFrame(ctx, t, antenne).(*lproto.Delta); !ok {
		t.Fatalf("antenne must receive its own scene's delta (a non-Delta here = preview leaked onto the antenne)")
	} else {
		var g string
		for _, p := range d.Patches {
			if p.Path == "score.team_a" {
				g = string(p.Value)
			}
		}
		if g != "21" {
			t.Fatalf("antenne delta = %q, want 21", g)
		}
	}

	// Terminal silence: after its legit delta, the antenne has NO queued frame
	// from the preview switch (any scene_changed here = the split is broken).
	expectNoFrame(t, antenne, 400*time.Millisecond, "antenne /show/stream.lsdp")
}
