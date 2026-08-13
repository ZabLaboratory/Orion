package runtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// helper: build a scene with one input → one output sink. The output
// node carries the versioned `core.output@1` id (the sink passthrough
// registered by NewComputeRegistry, ADR 004 §7.3) and writes its single
// inbound value to its `Path` leaf.
func passthroughScene(t *testing.T, id string) *Scene {
	t.Helper()
	graph := &compiler.Graph{
		SceneID:      id,
		SceneVersion: "sha256:test",
		Nodes: []compiler.GraphNode{
			{ID: "in.score", Kind: "input"},
			{ID: "out.score", Kind: "output", Path: "score.team_a", Compute: "core.output@1", Upstream: []string{"in.score"}},
		},
		Defaults: map[string]json.RawMessage{
			"score.team_a": json.RawMessage(`0`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}
	return NewScene(id, graph, bundle, NewComputeRegistry(), quietLogger())
}

// TestScene_DormantGatedSceneSkipsRecompute is ADR 008 §3.1 (issue #149,
// criterion #1): a gated roster instance that is OFF AIR does no dataflow
// recompute. A dataflow write to a declared leaf neither seeds the dirty
// cone nor produces a delta while the scene sits backstage; once it takes
// the antenna (on air) the same write recomputes and a delta flows. This
// is the dataflow counterpart of the on-tick/on-event firing gate proven
// in exec_onair_test.go.
func TestScene_DormantGatedSceneSkipsRecompute(t *testing.T) {
	scene := passthroughScene(t, "scene-dormant")
	scene.GateTriggers() // roster instance; off air by default (SeedOnAir not called)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(8)

	// Off air: a dataflow write must NOT produce a delta (no recompute).
	if !scene.Input(InputMsg{Path: "score.team_a", Value: json.RawMessage(`14`), Source: "operator:u", ClientMsgID: "off-1"}) {
		t.Fatal("inbox full")
	}
	select {
	case msg := <-sub.Out:
		if d, ok := msg.(*protocol.Delta); ok && len(d.Patches) > 0 {
			t.Fatalf("dormant off-air scene emitted a delta %+v — recompute ran backstage", d.Patches)
		}
	case <-time.After(300 * time.Millisecond):
		// No delta — the expected outcome.
	}

	// On air: the same write now recomputes and a delta flows.
	if !scene.SetOnAir(true) {
		t.Fatal("inbox full setting on-air")
	}
	if !scene.Input(InputMsg{Path: "score.team_a", Value: json.RawMessage(`21`), Source: "operator:u", ClientMsgID: "on-1"}) {
		t.Fatal("inbox full")
	}
	deadline := time.After(time.Second)
	for {
		select {
		case msg := <-sub.Out:
			if d, ok := msg.(*protocol.Delta); ok && len(d.Patches) > 0 {
				return // delta observed on air — pass
			}
		case <-deadline:
			t.Fatal("on-air scene produced no delta after dataflow write")
		}
	}
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

// panicMirror simulates a buggy / 3rd-party kit wire that panics on
// every emit. The bespoke wire (source of truth) must survive — both
// the SetMirror seed and the fan-out tap are recover-isolated
// (ADR 007 §C.3b, Vigil #26 medium).
type panicMirror struct{}

func (panicMirror) Forward(SubscriberMsg) { panic("boom from kit") }

func TestScene_MirrorPanicDoesNotKillBespoke(t *testing.T) {
	scene := passthroughScene(t, "scene-panic")
	scene.SetMirror(panicMirror{}) // seed via tapMirror must not panic the caller
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(8)
	if !scene.Input(InputMsg{
		Path:        "score.team_a",
		Value:       json.RawMessage(`14`),
		Source:      "operator:u",
		ClientMsgID: "uuid-panic",
	}) {
		t.Fatal("inbox full?")
	}

	select {
	case msg := <-sub.Out:
		if _, ok := msg.(*protocol.Delta); !ok {
			t.Fatalf("expected Delta despite mirror panic, got %T", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("bespoke wire died after mirror panic — recover isolation failed")
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

// TestScene_VersionedBlueprintGraphWritesLeaf exercises the §7.3 fix on
// a *real* versioned graph built only from NewComputeRegistry() — no
// custom Register(). It models a blueprint-bearing scene:
//
//	core.literal@1 (seeded calc.base=10)  ┐
//	                                       ├─ core.math.add@1 → calc.sum
//	core.input@1   (calc.delta, adapter)  ┘
//	                                          └─ core.output@1 → score.team_a
//
// Pushing calc.delta=5 must make the output sink leaf score.team_a equal
// 15. Before the re-key, Get("core.math.add@1") missed → the add node's
// leaf was never written → score.team_a stalled at its default. With the
// registry keyed on namespace.name@version, the whole chain resolves.
func TestScene_VersionedBlueprintGraphWritesLeaf(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "scene-versioned",
		SceneVersion: "sha256:versioned",
		Nodes: []compiler.GraphNode{
			// literal seeds calc.base; lands Kind=input (no upstream),
			// skipped by recompute, value comes from Defaults (§7.3).
			{ID: "lit.base", Kind: "input", Path: "calc.base", Compute: "core.literal@1"},
			// adapter-written input leaf.
			{ID: "in.delta", Kind: "input", Path: "calc.delta", Compute: "core.input@1"},
			// add reads its two upstreams positionally as ports a,b.
			{ID: "add", Kind: "computed", Path: "calc.sum", Compute: "core.math.add@1", Upstream: []string{"lit.base", "in.delta"}},
			// output sink passes calc.sum through to the public leaf.
			{ID: "out", Kind: "output", Path: "score.team_a", Compute: "core.output@1", Upstream: []string{"add"}},
		},
		Defaults: map[string]json.RawMessage{
			"calc.base":    json.RawMessage(`10`),
			"calc.delta":   json.RawMessage(`0`),
			"calc.sum":     json.RawMessage(`0`),
			"score.team_a": json.RawMessage(`0`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:versioned"}
	scene := NewScene("scene-versioned", graph, bundle, NewComputeRegistry(), quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(16)

	// Adapter writes calc.delta = 5. Expected: calc.sum = 15 and the
	// output sink leaf score.team_a = 15.
	if !scene.Input(InputMsg{Path: "calc.delta", Value: json.RawMessage(`5`), Source: "test"}) {
		t.Fatal("inbox full?")
	}

	got := map[string]json.RawMessage{}
	deadline := time.After(time.Second)
	for {
		done := false
		select {
		case msg := <-sub.Out:
			d, ok := msg.(*protocol.Delta)
			if !ok {
				continue
			}
			for _, p := range d.Patches {
				got[p.Path] = p.Value
			}
			// Wait until the sink leaf has propagated.
			if _, ok := got["score.team_a"]; ok {
				done = true
			}
		case <-deadline:
			t.Fatalf("timed out; patches so far: %v", got)
		}
		if done {
			break
		}
	}

	if string(got["score.team_a"]) != "15" {
		t.Fatalf("output sink leaf score.team_a = %s, want 15 (the add@1 result through output@1)", got["score.team_a"])
	}
	if v, ok := got["calc.sum"]; ok && string(v) != "15" {
		t.Fatalf("calc.sum = %s, want 15", v)
	}
}

// TestScene_ColdStartComputesBlueprintLeaf proves the two runtime gaps the
// blueprint path hit (found 2026-06-06, masked by hand-built test graphs):
//
//   - COLD-START COMPUTE: a computed leaf must be evaluated before the first
//     Subscribe, so a blueprint-backed scene renders its computed value on
//     go-live WITHOUT waiting for an input. (Seed only fills constants; the
//     loop only recomputed on an input.)
//   - INTERMEDIATE PERSISTENCE: an intermediate compute carries Path=="" from
//     the REAL compiler (nodeLeafPath returns "" for core.math.*); its result
//     must still be written (to its node id) so the downstream output sink can
//     read it. The prior code only wrote when Path!="", so any compiler-shaped
//     multi-stage graph chained to null.
//
// Graph (exactly as compiler.validateBlueprint shapes it — note add.Path==""):
//
//	core.literal@1 lit.a (Defaults["lit.a"]=10)  ┐
//	core.literal@1 lit.b (Defaults["lit.b"]=5)   ├─ core.math.add@1 add (Path="")
//	                                              └─ core.output@1 out → display.total
//
// No input is pushed. The very first snapshot must carry display.total = 15.
func TestScene_ColdStartComputesBlueprintLeaf(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "scene-cold",
		SceneVersion: "sha256:cold",
		Nodes: []compiler.GraphNode{
			{ID: "lit.a", Kind: "input", Path: "lit.a", Compute: "core.literal@1"},
			{ID: "lit.b", Kind: "input", Path: "lit.b", Compute: "core.literal@1"},
			// Intermediate: Path=="" exactly like the real compiler emits.
			{ID: "add", Kind: "computed", Path: "", Compute: "core.math.add@1", Upstream: []string{"lit.a", "lit.b"}},
			{ID: "out", Kind: "output", Path: "display.total", Compute: "core.output@1", Upstream: []string{"add"}},
		},
		Defaults: map[string]json.RawMessage{
			"lit.a": json.RawMessage(`10`),
			"lit.b": json.RawMessage(`5`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:cold"}
	scene := NewScene("scene-cold", graph, bundle, NewComputeRegistry(), quietLogger())

	// Subscribe WITHOUT pushing any input — the snapshot must already carry
	// the cold-start-computed output leaf.
	_, snap := scene.Subscribe(8)
	if got := string(snap.State["display.total"]); got != "15" {
		t.Fatalf("cold-start display.total = %q, want 15 (literal 10 + literal 5, computed before first subscribe)", got)
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

// TestScene_ConcurrentCloseDuringFanoutNeverPanics regression-tests the
// #331 CI finding (run 31729211951, TestOperator_CallRuleSelectorFires):
// fanout snapshots the subscriber list outside subsMu, so a concurrent
// Subscription.Close can run between that snapshot and fanout's send —
// without Subscription.mu serializing trySend/drainAndSeed against
// Close's close(Out), this is a real send-on-a-closing-channel race
// (panics), not merely a -race false positive. A tiny buffer (size 1)
// maximizes the chance fanout hits the full-queue collapse path, the
// other guarded call site.
func TestScene_ConcurrentCloseDuringFanoutNeverPanics(t *testing.T) {
	scene := passthroughScene(t, "scene-race")
	scene.SeedOnAir(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	const subscribers = 20
	subs := make([]*Subscription, subscribers)
	for i := range subs {
		sub, _ := scene.Subscribe(1)
		subs[i] = sub
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			scene.Input(InputMsg{
				Path: "score.team_a", Value: json.RawMessage(`14`),
				Source: "operator:u", ClientMsgID: "race",
			})
		}
	}()

	var wg sync.WaitGroup
	for _, sub := range subs {
		wg.Add(1)
		go func(s *Subscription) {
			defer wg.Done()
			// Drain a little so some sends succeed via trySend before
			// this subscriber closes concurrently with the emitter.
			select {
			case <-s.Out:
			default:
			}
			s.Close()
		}(sub)
	}

	<-done
	wg.Wait()
}
