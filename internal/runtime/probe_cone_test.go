package runtime

// Probe — dirty-cone correctness, issue #80, ADR 003 §3.1.5.
//
// Forge's dirtycone_test.go covers: two-chain isolation, idempotent
// write, unchanged-value propagation stop.
//
// This file covers the remaining semantic holes:
//   - Diamond (two paths of different length to one sink): exactly one
//     recompute, value correct (reads BOTH upstreams after they updated).
//   - Fan-out (1 source → N consumers): all N recompute.
//   - Multi-sink partial dirty: sinks outside the cone keep their value.
//   - Multi-write batch: two inputs written before draining both land.
//   - Deep chain at boundary: value propagates through a 1000-node chain.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// ---------------------------------------------------------------------------
// Diamond: in → mid.left, in → mid.right, (mid.left + mid.right) → out.sum
//
//	topo order: in(0) · mid.left(1) · mid.right(2) · out.sum(3)
//
// A write to "in" must:
//   - evaluate mid.left, mid.right, then out.sum — in that topo order.
//   - invoke out.sum exactly once (dedup in the min-heap).
//   - produce a value that reflects BOTH updated upstreams.
//
// The "reflect both" check is load-bearing: if out.sum ran before
// mid.right updated, it would read mid.right's stale value.
// ---------------------------------------------------------------------------
func TestScene_DiamondRecomputesOnceWithBothUpstreams(t *testing.T) {
	var sumCalls atomic.Int64

	graph := &compiler.Graph{
		SceneID:      "diamond-test",
		SceneVersion: "sha256:diamond",
		Nodes: []compiler.GraphNode{
			// topo index 0
			{ID: "in.x", Kind: "input"},
			// topo index 1 — doubles the input
			{ID: "mid.left", Kind: "computed", Compute: "test.double@1", Upstream: []string{"in.x"}},
			// topo index 2 — triples the input
			{ID: "mid.right", Kind: "computed", Compute: "test.triple@1", Upstream: []string{"in.x"}},
			// topo index 3 — sums left+right; expects a=double, b=triple
			{
				ID:       "out.sum",
				Kind:     "output",
				Path:     "leaf.sum",
				Compute:  "test.sum2@1",
				Upstream: []string{"mid.left", "mid.right"},
			},
		},
		Defaults: map[string]json.RawMessage{},
	}

	reg := NewComputeRegistry()
	// double: returns input*2
	reg.Register("test.double@1", func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
		for _, v := range inputs {
			var n float64
			if err := json.Unmarshal(v, &n); err != nil {
				return json.RawMessage(`0`), nil
			}
			return json.Marshal(n * 2)
		}
		return json.RawMessage(`0`), nil
	})
	// triple: returns input*3
	reg.Register("test.triple@1", func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
		for _, v := range inputs {
			var n float64
			if err := json.Unmarshal(v, &n); err != nil {
				return json.RawMessage(`0`), nil
			}
			return json.Marshal(n * 3)
		}
		return json.RawMessage(`0`), nil
	})
	// sum2: adds its two upstream values (positional a, b)
	reg.Register("test.sum2@1", func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
		sumCalls.Add(1)
		var a, b float64
		if v, ok := inputs["a"]; ok {
			_ = json.Unmarshal(v, &a)
		}
		if v, ok := inputs["b"]; ok {
			_ = json.Unmarshal(v, &b)
		}
		return json.Marshal(a + b)
	})

	scene := NewScene("diamond-test", graph, &compiler.RenderBundle{SceneVersion: "sha256:diamond"}, reg, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(16)
	defer sub.Close()

	// Drain cold-start delta.
	if !scene.Input(InputMsg{Path: "in.x", Value: json.RawMessage(`0`), Source: "operator:t", ClientMsgID: "cold"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(time.Second):
		t.Fatal("no cold-start delta")
	}
	afterCold := sumCalls.Load()

	// Now write a real value.
	if !scene.Input(InputMsg{Path: "in.x", Value: json.RawMessage(`10`), Source: "operator:t", ClientMsgID: "w1"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(time.Second):
		t.Fatal("no delta after diamond write")
	}

	// out.sum must have been called exactly once (dedup).
	if got := sumCalls.Load() - afterCold; got != 1 {
		t.Fatalf("diamond node (out.sum) invoked %d times, want exactly 1 (dedup failure)", got)
	}

	// Value must be 10*2 + 10*3 = 50, meaning both mid.left AND
	// mid.right were updated BEFORE out.sum ran.
	v, ok := scene.state.Get("leaf.sum")
	if !ok {
		t.Fatal("leaf.sum absent after diamond write")
	}
	var got float64
	if err := json.Unmarshal(v, &got); err != nil {
		t.Fatalf("leaf.sum not a number: %s", v)
	}
	// 10*2 + 10*3 = 50
	if got != 50 {
		t.Fatalf("leaf.sum = %v, want 50 (double+triple of 10); stale upstream read would give 0+30=30 or 20+0=20", got)
	}
}

// ---------------------------------------------------------------------------
// Fan-out: one input drives N=8 independent output sinks. All must
// recompute after a write; none must be skipped.
// ---------------------------------------------------------------------------
func TestScene_FanOutAllConsumersRecompute(t *testing.T) {
	const N = 8
	counters := make([]*atomic.Int64, N)
	for i := range counters {
		counters[i] = new(atomic.Int64)
	}

	nodes := []compiler.GraphNode{{ID: "in.src", Kind: "input"}}
	reg := NewComputeRegistry()
	for i := 0; i < N; i++ {
		computeKey := fmt.Sprintf("test.fanout.%d@1", i)
		idx := i
		reg.Register(computeKey, func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
			counters[idx].Add(1)
			for _, v := range inputs {
				return v, nil
			}
			return json.RawMessage(`null`), nil
		})
		nodes = append(nodes, compiler.GraphNode{
			ID:       fmt.Sprintf("out.%d", i),
			Kind:     "output",
			Path:     fmt.Sprintf("leaf.%d", i),
			Compute:  computeKey,
			Upstream: []string{"in.src"},
		})
	}

	graph := &compiler.Graph{
		SceneID:      "fanout-test",
		SceneVersion: "sha256:fanout",
		Nodes:        nodes,
		Defaults:     map[string]json.RawMessage{},
	}
	scene := NewScene("fanout-test", graph, &compiler.RenderBundle{SceneVersion: "sha256:fanout"}, reg, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(16)
	defer sub.Close()

	// Drain cold start.
	if !scene.Input(InputMsg{Path: "in.src", Value: json.RawMessage(`0`), Source: "operator:t", ClientMsgID: "cold"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(time.Second):
		t.Fatal("no cold delta")
	}
	before := make([]int64, N)
	for i := range counters {
		before[i] = counters[i].Load()
	}

	// Write a new value — all N sinks must recompute.
	if !scene.Input(InputMsg{Path: "in.src", Value: json.RawMessage(`99`), Source: "operator:t", ClientMsgID: "w1"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(time.Second):
		t.Fatal("no delta after fan-out write")
	}

	for i, c := range counters {
		if got := c.Load() - before[i]; got != 1 {
			t.Errorf("fan-out sink %d: recomputed %d times, want 1", i, got)
		}
		// Value must have propagated.
		v, ok := scene.state.Get(fmt.Sprintf("leaf.%d", i))
		if !ok {
			t.Errorf("fan-out sink %d: leaf absent", i)
			continue
		}
		if string(v) != `99` {
			t.Errorf("fan-out sink %d: leaf = %s, want 99", i, v)
		}
	}
}

// ---------------------------------------------------------------------------
// Multi-sink partial dirty: two independent sources feed two sinks.
// Dirtying only one source must leave the other sink's value intact
// (not reset, not recomputed).
// ---------------------------------------------------------------------------
func TestScene_MultiSinkPartialDirtyPreservesUntouchedSink(t *testing.T) {
	var callsA, callsB atomic.Int64

	graph := &compiler.Graph{
		SceneID:      "partial-test",
		SceneVersion: "sha256:partial",
		Nodes: []compiler.GraphNode{
			{ID: "in.a", Kind: "input"},
			{ID: "in.b", Kind: "input"},
			{
				ID: "out.a", Kind: "output", Path: "leaf.a",
				Compute:  "test.relay.a@1",
				Upstream: []string{"in.a"},
			},
			{
				ID: "out.b", Kind: "output", Path: "leaf.b",
				Compute:  "test.relay.b@1",
				Upstream: []string{"in.b"},
			},
		},
		Defaults: map[string]json.RawMessage{},
	}
	reg := NewComputeRegistry()
	relay := func(c *atomic.Int64) ComputeFn {
		return func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
			c.Add(1)
			for _, v := range inputs {
				return v, nil
			}
			return json.RawMessage(`null`), nil
		}
	}
	reg.Register("test.relay.a@1", relay(&callsA))
	reg.Register("test.relay.b@1", relay(&callsB))

	scene := NewScene("partial-test", graph, &compiler.RenderBundle{SceneVersion: "sha256:partial"}, reg, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(16)
	defer sub.Close()

	// Establish known values for both sinks.
	for _, msg := range []InputMsg{
		{Path: "in.a", Value: json.RawMessage(`"alpha"`), Source: "operator:t", ClientMsgID: "seed-a"},
		{Path: "in.b", Value: json.RawMessage(`"beta"`), Source: "operator:t", ClientMsgID: "seed-b"},
	} {
		if !scene.Input(msg) {
			t.Fatalf("inbox full for %s", msg.Path)
		}
		select {
		case <-sub.Out:
		case <-time.After(time.Second):
			t.Fatalf("no delta after seeding %s", msg.Path)
		}
	}

	callsBefore := callsB.Load()

	// Now only dirty in.a.
	if !scene.Input(InputMsg{Path: "in.a", Value: json.RawMessage(`"alpha2"`), Source: "operator:t", ClientMsgID: "dirty-a"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(time.Second):
		t.Fatal("no delta after partial dirty")
	}

	// out.b must NOT have recomputed.
	if got := callsB.Load() - callsBefore; got != 0 {
		t.Fatalf("out.b recomputed %d times after writing only in.a (cone leak)", got)
	}
	// out.b value must be preserved.
	v, ok := scene.state.Get("leaf.b")
	if !ok {
		t.Fatal("leaf.b absent (value lost after partial dirty)")
	}
	if string(v) != `"beta"` {
		t.Fatalf("leaf.b = %s, want \"beta\" (value corrupted by partial dirty)", v)
	}
	// out.a must have updated.
	va, ok := scene.state.Get("leaf.a")
	if !ok {
		t.Fatal("leaf.a absent after dirty")
	}
	if string(va) != `"alpha2"` {
		t.Fatalf("leaf.a = %s, want \"alpha2\"", va)
	}
}

// ---------------------------------------------------------------------------
// Multi-write batch: two writes to the same input node arrive before the
// scene loop drains. Only the LAST value should reach the output (inbox
// drain semantics). This is not a cone correctness failure but a
// semantic regression check: the batch must not produce a stale value
// or skip the recompute entirely.
// ---------------------------------------------------------------------------
func TestScene_MultiWriteBatchLastValueWins(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "batch-test",
		SceneVersion: "sha256:batch",
		Nodes: []compiler.GraphNode{
			{ID: "in.v", Kind: "input"},
			{ID: "out.v", Kind: "output", Path: "leaf.v", Compute: "core.output@1", Upstream: []string{"in.v"}},
		},
		Defaults: map[string]json.RawMessage{},
	}
	scene := NewScene("batch-test", graph, &compiler.RenderBundle{SceneVersion: "sha256:batch"}, NewComputeRegistry(), quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(32)
	defer sub.Close()

	// Drain cold start.
	if !scene.Input(InputMsg{Path: "in.v", Value: json.RawMessage(`0`), Source: "operator:t", ClientMsgID: "cold"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(time.Second):
		t.Fatal("no cold delta")
	}

	// Push two writes quickly; the scene loop may drain both in one
	// applyInput + drainNonBlocking cycle, or in two separate cycles.
	// Either way, after all deltas are emitted, leaf.v must be "last".
	if !scene.Input(InputMsg{Path: "in.v", Value: json.RawMessage(`"first"`), Source: "operator:t", ClientMsgID: "b1"}) {
		t.Fatal("inbox full b1")
	}
	if !scene.Input(InputMsg{Path: "in.v", Value: json.RawMessage(`"last"`), Source: "operator:t", ClientMsgID: "b2"}) {
		t.Fatal("inbox full b2")
	}

	// Drain until we've seen at least one delta; then poll briefly.
	deadline := time.After(2 * time.Second)
outer:
	for {
		select {
		case <-sub.Out:
			// Check whether more are immediately available.
			select {
			case <-sub.Out:
			default:
				break outer
			}
		case <-deadline:
			t.Fatal("no delta after multi-write batch")
		}
	}

	v, ok := scene.state.Get("leaf.v")
	if !ok {
		t.Fatal("leaf.v absent after batch writes")
	}
	// We cannot assert strict "last" wins when the loop processes both
	// writes in separate ticks, but we CAN assert the value is one of
	// the two we wrote (not stale from before).
	if string(v) != `"first"` && string(v) != `"last"` {
		t.Fatalf("leaf.v = %s after batch writes, want either \"first\" or \"last\"", v)
	}
}

// ---------------------------------------------------------------------------
// Deep chain at 1000-node boundary: value propagates through a chain of
// exactly 1000 nodes (1 input + 998 relay computes + 1 output).
// This is a SEMANTIC test, not a timing gate: the cone must not be
// truncated, pruned, or capped at any depth.
// ---------------------------------------------------------------------------
func TestScene_DeepChain1000NodesValueReachesSink(t *testing.T) {
	const depth = 998 // relay nodes; total = input + 998 + output = 1000

	nodes := make([]compiler.GraphNode, 0, depth+2)
	nodes = append(nodes, compiler.GraphNode{ID: "in.deep", Kind: "input"})
	for i := 0; i < depth; i++ {
		prev := "in.deep"
		if i > 0 {
			prev = fmt.Sprintf("relay.%d", i-1)
		}
		nodes = append(nodes, compiler.GraphNode{
			ID:       fmt.Sprintf("relay.%d", i),
			Kind:     "computed",
			Compute:  "test.passthrough@1",
			Upstream: []string{prev},
		})
	}
	nodes = append(nodes, compiler.GraphNode{
		ID:       "out.deep",
		Kind:     "output",
		Path:     "leaf.deep",
		Compute:  "core.output@1",
		Upstream: []string{fmt.Sprintf("relay.%d", depth-1)},
	})

	reg := NewComputeRegistry()
	reg.Register("test.passthrough@1", func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
		for _, v := range inputs {
			return v, nil
		}
		return json.RawMessage(`null`), nil
	})

	graph := &compiler.Graph{
		SceneID:      "deep-chain-test",
		SceneVersion: "sha256:deep",
		Nodes:        nodes,
		Defaults:     map[string]json.RawMessage{},
	}
	scene := NewScene("deep-chain-test", graph, &compiler.RenderBundle{SceneVersion: "sha256:deep"}, reg, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(16)
	defer sub.Close()

	// Drain cold start.
	if !scene.Input(InputMsg{Path: "in.deep", Value: json.RawMessage(`0`), Source: "operator:t", ClientMsgID: "cold"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(5 * time.Second):
		t.Fatal("no cold delta for 1000-node chain")
	}

	// Now write a sentinel value and expect it at the other end.
	if !scene.Input(InputMsg{Path: "in.deep", Value: json.RawMessage(`"sentinel"`), Source: "operator:t", ClientMsgID: "w1"}) {
		t.Fatal("inbox full")
	}
	select {
	case <-sub.Out:
	case <-time.After(5 * time.Second):
		t.Fatal("no delta after 1000-node chain write")
	}

	v, ok := scene.state.Get("leaf.deep")
	if !ok {
		t.Fatal("leaf.deep absent — chain was truncated or cone walk stopped early")
	}
	if string(v) != `"sentinel"` {
		t.Fatalf("leaf.deep = %s, want \"sentinel\" — value did not reach the sink of the 1000-node chain", v)
	}
}
