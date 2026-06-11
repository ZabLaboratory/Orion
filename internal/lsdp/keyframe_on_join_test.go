package lsdp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	lproto "github.com/Lumencast/lumencast-go/protocol"

	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// TestLSDP_LateJoinAfterReactiveUpdateGetsKeyframe is the prod
// regression (Pulsar always joins late): a value is produced reactively
// AFTER activation, while NO LSDP subscriber is attached. A subscriber
// that connects only afterwards MUST still receive a keyframe (snapshot)
// carrying scene_id + scene_version + the current state, so Solar can
// learn the scene and fetch the render bundle.
//
// This is the exact failure that left Solar on a black screen: the kit
// store must reflect every reactive emit that happened while no LSDP
// subscriber was attached, and the join snapshot must carry it.
func TestLSDP_LateJoinAfterReactiveUpdateGetsKeyframe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, scene, wsURL := dualShow(t)

	// Reactive update AFTER activation, with NO LSDP subscriber attached
	// (mirrors the leaderboard board being computed before Pulsar joins).
	if !scene.Input(runtime.InputMsg{
		Path:  "score.team_a",
		Value: json.RawMessage(`9`),
	}) {
		t.Fatal("scene inbox full")
	}
	// The emit lands in the kit store via the mirror tap even though no
	// LSDP subscriber exists yet.
	waitForState(t, wire, scene, "score.team_a", "9")

	// Now Pulsar joins late. It must receive a keyframe snapshot with the
	// post-activation value, not an empty / stale state.
	c := dialLSDP(ctx, t, wsURL, "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")

	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatalf("late join: first frame must be a snapshot keyframe")
	}
	if snap.SceneID != "scene-1" {
		t.Fatalf("keyframe scene_id = %q, want scene-1", snap.SceneID)
	}
	// @lumencast/runtime rejects a join snapshot with seq < 1 (VERSION_GAP
	// → onError → black screen). The keyframe MUST carry seq >= 1.
	if snap.Seq < 1 {
		t.Fatalf("keyframe seq = %d, want >= 1 (runtime rejects seq<1 as VERSION_GAP)", snap.Seq)
	}
	if snap.SceneVersion != "sha256:test-1" {
		t.Fatalf("keyframe scene_version = %q, want sha256:test-1", snap.SceneVersion)
	}
	if string(snap.State["score.team_a"]) != "9" {
		t.Fatalf("keyframe state[score.team_a] = %q, want 9 (board lost)", snap.State["score.team_a"])
	}

	// Deltas keep flowing after the keyframe.
	if !scene.Input(runtime.InputMsg{
		Path:  "score.team_a",
		Value: json.RawMessage(`14`),
	}) {
		t.Fatal("scene inbox full")
	}
	delta, ok := readServerFrame(ctx, t, c).(*lproto.Delta)
	if !ok {
		t.Fatalf("post-keyframe frame must be a delta")
	}
	var got string
	for _, p := range delta.Patches {
		if p.Path == "score.team_a" {
			got = string(p.Value)
		}
	}
	if got != "14" {
		t.Fatalf("delta patch = %q, want 14", got)
	}
}
