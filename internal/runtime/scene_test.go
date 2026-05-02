package runtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// helper: build a scene with one input → one output passthrough.
func passthroughScene(t *testing.T, id string) *Scene {
	t.Helper()
	graph := &compiler.Graph{
		SceneID:      id,
		SceneVersion: "sha256:test",
		Nodes: []compiler.GraphNode{
			{ID: "in.score", Kind: "input"},
			{ID: "out.score", Kind: "output", Path: "score.team_a", Compute: "core.passthrough", Upstream: []string{"in.score"}},
		},
		Defaults: map[string]json.RawMessage{
			"score.team_a": json.RawMessage(`0`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}
	return NewScene(id, graph, bundle, NewComputeRegistry(), quietLogger())
}

func TestScene_InputProducesDelta(t *testing.T) {
	scene := passthroughScene(t, "scene-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, snap := scene.Subscribe(8)
	if snap.SceneID != "scene-1" {
		t.Fatal("snapshot scene_id wrong")
	}

	start := time.Now()
	if !scene.Input(InputMsg{
		Path:        "score.team_a",
		Value:       json.RawMessage(`14`),
		Source:      "operator:user-abc",
		ClientMsgID: "uuid-1",
	}) {
		t.Fatal("inbox full?")
	}

	select {
	case msg := <-sub.Out:
		latency := time.Since(start)
		// ADR 004 criterion 4: ≤ 50 ms input-to-delta on dev machine.
		if latency > 50*time.Millisecond {
			t.Errorf("input-to-delta latency %v exceeds 50ms", latency)
		}
		d, ok := msg.(*protocol.Delta)
		if !ok {
			t.Fatalf("expected Delta, got %T", msg)
		}
		if len(d.Patches) == 0 {
			t.Fatal("delta empty")
		}
		if d.Cause == nil || d.Cause.InputID != "uuid-1" {
			t.Fatalf("cause not echoed: %+v", d.Cause)
		}
	case <-time.After(time.Second):
		t.Fatal("no delta received")
	}
}

func TestScene_BurstCoalescesIntoOneRecompute(t *testing.T) {
	scene := passthroughScene(t, "scene-burst")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(64)

	// Push 50 inputs as fast as possible. Drain-then-compute should
	// coalesce them all into 1-2 deltas, not 50.
	for i := 0; i < 50; i++ {
		v, _ := json.Marshal(i)
		scene.Input(InputMsg{Path: "score.team_a", Value: v, Source: "test"})
	}

	deadline := time.After(250 * time.Millisecond)
	deltas := 0
	for {
		select {
		case msg := <-sub.Out:
			if _, ok := msg.(*protocol.Delta); ok {
				deltas++
			}
		case <-deadline:
			if deltas == 0 {
				t.Fatal("no delta after burst")
			}
			if deltas >= 50 {
				t.Errorf("burst produced %d deltas — drain-then-compute didn't coalesce", deltas)
			}
			return
		}
	}
}

func TestScene_IdempotentInputProducesZeroPatchEcho(t *testing.T) {
	scene := passthroughScene(t, "scene-idem")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(8)
	// First write changes the value — produces a real delta.
	scene.Input(InputMsg{Path: "score.team_a", Value: json.RawMessage(`5`), Source: "x"})
	<-sub.Out

	// Second write of the same value with a client_msg_id — empty
	// patches, but Cause echoes back (ADR 002 § 6).
	scene.Input(InputMsg{
		Path:        "score.team_a",
		Value:       json.RawMessage(`5`),
		Source:      "x",
		ClientMsgID: "echo-me",
	})
	select {
	case msg := <-sub.Out:
		d := msg.(*protocol.Delta)
		if len(d.Patches) != 0 {
			t.Fatalf("expected zero-patch echo, got %d patches", len(d.Patches))
		}
		if d.Cause == nil || d.Cause.InputID != "echo-me" {
			t.Fatalf("cause echo missing: %+v", d.Cause)
		}
	case <-time.After(time.Second):
		t.Fatal("no zero-patch echo")
	}
}

func TestShow_SwitchMigratesLiveSubsAndEmitsSceneChanged(t *testing.T) {
	logger := quietLogger()
	show := NewShow(NewComputeRegistry(), logger)
	t.Cleanup(show.Stop)

	a := passthroughScene(t, "scene-a")
	b := passthroughScene(t, "scene-b")
	show.Load("scene-a", a.graph, a.bundle)
	show.Load("scene-b", b.graph, b.bundle)

	// Operator's first activate brings scene-a alive.
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}

	sub, snap, err := show.SubscribeLive(8)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SceneID != "scene-a" {
		t.Fatalf("initial snap scene_id %q", snap.SceneID)
	}

	// Switch to scene-b. The live subscriber must receive
	// scene_changed (from a to b) followed by a fresh snapshot of b.
	if err := show.SetActive("scene-b", nil); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(time.Second)
	gotChanged := false
	gotSnap := false
	for !(gotChanged && gotSnap) {
		select {
		case msg := <-sub.Out:
			switch m := msg.(type) {
			case *protocol.SceneChanged:
				if m.FromSceneID != "scene-a" || m.ToSceneID != "scene-b" {
					t.Fatalf("changed %s→%s", m.FromSceneID, m.ToSceneID)
				}
				gotChanged = true
			case *protocol.Snapshot:
				if m.SceneID != "scene-b" {
					t.Fatalf("snap scene_id %q", m.SceneID)
				}
				gotSnap = true
			}
		case <-deadline:
			t.Fatalf("timed out, gotChanged=%v gotSnap=%v", gotChanged, gotSnap)
		}
	}
}
