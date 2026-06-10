package runtime

// Dirty-cone recompute correctness — issue #80, ADR 003 §3.1.5.
// The perf gate (perf20k_test.go) proves the budgets; these tests
// prove the SEMANTICS: a write recomputes exactly the affected cone,
// chains propagate through intermediates in topo order, and an
// unchanged computed value stops the propagation (same behaviour the
// full-walk IsDirty implementation had).

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// twoChainScene builds two INDEPENDENT chains in one scene:
//
//	in.a → mid.a (count.a) → out.a   |   in.b → mid.b (count.b) → out.b
//
// with per-chain invocation counters injected via the registry.
func twoChainScene(t *testing.T) (*Scene, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	graph := &compiler.Graph{
		SceneID:      "cone-test",
		SceneVersion: "sha256:cone",
		Nodes: []compiler.GraphNode{
			{ID: "in.a", Kind: "input"},
			{ID: "in.b", Kind: "input"},
			{ID: "mid.a", Kind: "computed", Compute: "test.count.a@1", Upstream: []string{"in.a"}},
			{ID: "mid.b", Kind: "computed", Compute: "test.count.b@1", Upstream: []string{"in.b"}},
			{ID: "out.a", Kind: "output", Path: "leaf.a", Compute: "core.output@1", Upstream: []string{"mid.a"}},
			{ID: "out.b", Kind: "output", Path: "leaf.b", Compute: "core.output@1", Upstream: []string{"mid.b"}},
		},
		Defaults: map[string]json.RawMessage{},
	}
	var countA, countB atomic.Int64
	reg := NewComputeRegistry()
	counting := func(c *atomic.Int64) ComputeFn {
		return func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
			c.Add(1)
			for _, v := range inputs {
				return v, nil
			}
			return json.RawMessage(`null`), nil
		}
	}
	reg.Register("test.count.a@1", counting(&countA))
	reg.Register("test.count.b@1", counting(&countB))
	scene := NewScene("cone-test", graph, &compiler.RenderBundle{SceneVersion: "sha256:cone"}, reg, quietLogger())
	return scene, &countA, &countB
}

// A write to chain A must never evaluate chain B's computes — the
// walk's cost is the cone, not the scene.
func TestScene_RecomputeWalksDirtyConeOnly(t *testing.T) {
	scene, countA, countB := twoChainScene(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(8)
	defer sub.Close()

	// Flush the cold-start dirty bits out of the first delta (the
	// scaffold has always carried them there — FlushDirty only runs at
	// the first emit) so the assertion below sees ONLY the cone's
	// patches.
	if !scene.Input(InputMsg{Path: "in.b", Value: json.RawMessage(`1`), Source: "operator:t"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(time.Second):
		t.Fatal("no warm-up delta")
	}

	// Cold start + warm-up ran; count deltas from here.
	coldA, coldB := countA.Load(), countB.Load()

	if !scene.Input(InputMsg{Path: "in.a", Value: json.RawMessage(`42`), Source: "operator:t"}) {
		t.Fatal("inbox full")
	}
	select {
	case msg := <-sub.Out:
		d, ok := msg.(*protocol.Delta)
		if !ok {
			t.Fatalf("expected Delta, got %T", msg)
		}
		for _, p := range d.Patches {
			if p.Path == "leaf.b" || p.Path == "mid.b" {
				t.Fatalf("write to chain A patched chain B path %q", p.Path)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("no delta")
	}

	if got := countA.Load() - coldA; got != 1 {
		t.Fatalf("chain A compute invoked %d times for one write, want 1", got)
	}
	if got := countB.Load() - coldB; got != 0 {
		t.Fatalf("chain B compute invoked %d times for a chain-A write, want 0 (cone leak)", got)
	}

	// And the value actually landed (the cone walk served the chain,
	// it did not just skip work).
	if v, ok := scene.state.Get("leaf.a"); !ok || string(v) != `42` {
		t.Fatalf("leaf.a = %s (ok=%v), want 42", v, ok)
	}
}

// An idempotent input (same value) must trigger no recompute at all —
// the seed set is empty, matching the previous IsDirty behaviour.
func TestScene_RecomputeIdempotentWriteSeedsNothing(t *testing.T) {
	scene, countA, _ := twoChainScene(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(8)
	defer sub.Close()

	write := func(id string) {
		if !scene.Input(InputMsg{Path: "in.a", Value: json.RawMessage(`7`), Source: "operator:t", ClientMsgID: id}) {
			t.Fatal("inbox full")
		}
		select {
		case <-sub.Out:
		case <-time.After(time.Second):
			t.Fatal("no reply")
		}
	}
	write("first") // value change → recompute
	after := countA.Load()
	write("second") // identical value → zero-patch echo, zero recompute
	if got := countA.Load() - after; got != 0 {
		t.Fatalf("idempotent write triggered %d recomputes, want 0", got)
	}
}

// A compute whose result is UNCHANGED stops the downstream walk —
// pushed consumers only fire when their producer's value changed.
func TestScene_RecomputeUnchangedValueStopsPropagation(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "stop-test",
		SceneVersion: "sha256:stop",
		Nodes: []compiler.GraphNode{
			{ID: "in.x", Kind: "input"},
			{ID: "mid.const", Kind: "computed", Compute: "test.const@1", Upstream: []string{"in.x"}},
			{ID: "out.x", Kind: "output", Path: "leaf.x", Compute: "test.tail@1", Upstream: []string{"mid.const"}},
		},
		Defaults: map[string]json.RawMessage{},
	}
	var tailCalls atomic.Int64
	reg := NewComputeRegistry()
	reg.Register("test.const@1", func(_, _ map[string]json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"steady"`), nil // same value every time
	})
	reg.Register("test.tail@1", func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
		tailCalls.Add(1)
		for _, v := range inputs {
			return v, nil
		}
		return json.RawMessage(`null`), nil
	})
	scene := NewScene("stop-test", graph, &compiler.RenderBundle{SceneVersion: "sha256:stop"}, reg, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(8)
	defer sub.Close()

	afterCold := tailCalls.Load()
	if !scene.Input(InputMsg{Path: "in.x", Value: json.RawMessage(`1`), Source: "operator:t", ClientMsgID: "w1"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(time.Second):
		t.Fatal("no reply")
	}
	// mid.const recomputed (its upstream changed) but produced the same
	// value → out.x must NOT have been re-evaluated.
	if got := tailCalls.Load() - afterCold; got != 0 {
		t.Fatalf("downstream recomputed %d times behind an unchanged value, want 0", got)
	}
}
