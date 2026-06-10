package runtime

// 20 k-node synthetic benchmark scene — issue #80, ADR 003 §3.1.5 /
// resolution criterion 5 (phase-0 slice).
//
// The scene is GENERATED (deterministic code, not a multi-megabyte
// fixture): 20 independent "towers", each 1 operator input feeding a
// chain of 998 core.math.add@1 nodes (+1 per stage, via a shared
// core.literal@1) ending in a core.output@1 sink — 20 × 1000 + 1 =
// 20 001 nodes. A single-input write dirties exactly one tower's
// chain: a 999-node cone, the ADR's "cone ≤ 1 000" operating point.
//
// These tests are the CI REGRESSION GATE (no continue-on-error, no
// skip): they fail the suite if
//   - the 20 k blueprint no longer compiles, or
//   - cold-start full evaluation exceeds 2 s, or
//   - single-input → delta p95 exceeds 50 ms.
//
// Doctrine (ADR 003 §1.1): the gate asserts SPEED ONLY. The full
// 20 001-node graph is compiled, loaded and served — every assertion
// below would fail if the engine truncated, capped or skipped any
// part of it (the patch-count check proves the whole cone reached the
// wire).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

const (
	benchTowers     = 20  // independent input→chain→output towers
	benchChainAdds  = 998 // core.math.add@1 stages per tower
	benchConeBudget = 1000

	benchColdStartBudget = 2 * time.Second
	benchP95Budget       = 50 * time.Millisecond
	benchSamples         = 200
)

// skipUnderRace keeps the hard wall-clock gates out of the race
// build ONLY (race multiplies wall time 5–20× → guaranteed flakes).
// The gate itself stays armed: the dedicated `perf-20k` CI job runs
// these tests without -race and hard-fails the pipeline on any budget
// regression (no continue-on-error). Cone semantics still run under
// race via dirtycone_test.go.
func skipUnderRace(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("wall-clock gate runs in the dedicated no-race perf-20k CI job")
	}
}

// benchFetcher serves the generated 20 k blueprint through the real
// compiler entry point, so the gate covers compile, not just runtime.
type benchFetcher struct {
	layout    *compiler.CanvasLayout
	blueprint *compiler.BlueprintGraph
}

func (f *benchFetcher) FetchCanvasLayout(_ context.Context, _ string) (*compiler.CanvasLayout, error) {
	return f.layout, nil
}
func (f *benchFetcher) FetchBlueprint(_ context.Context, _ string) (*compiler.BlueprintGraph, error) {
	return f.blueprint, nil
}
func (f *benchFetcher) FetchComponent(_ context.Context, ref compiler.ComponentRef) (*compiler.UserComponent, error) {
	return nil, fmt.Errorf("bench scene has no components (asked for %s)", ref.ID)
}
func (f *benchFetcher) FetchComputeManifest(context.Context) (compiler.ComputeManifest, error) {
	return compiler.ComputeManifest{
		"core.input@1":    {IsPure: true, IsBounded: true, Version: "1"},
		"core.literal@1":  {IsPure: true, IsBounded: true, Version: "1"},
		"core.math.add@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":   {IsPure: true, IsBounded: true, Version: "1"},
	}, nil
}

// gen20kBlueprint builds the synthetic 20 001-node blueprint.
func gen20kBlueprint() *compiler.BlueprintGraph {
	bp := &compiler.BlueprintGraph{ID: "bench-20k"}
	one, _ := json.Marshal(1)
	bp.Nodes = append(bp.Nodes, compiler.BlueprintNode{
		ID:      "lit.one",
		Compute: "core.literal@1",
		Config:  map[string]json.RawMessage{"value": one},
	})
	for t := 0; t < benchTowers; t++ {
		inID := fmt.Sprintf("t%02d.in", t)
		bp.Nodes = append(bp.Nodes, compiler.BlueprintNode{
			ID:      inID,
			Compute: "core.input@1",
			Config:  map[string]json.RawMessage{"name": json.RawMessage(fmt.Sprintf("%q", benchInputPath(t)))},
		})
		prev := inID
		for c := 0; c < benchChainAdds; c++ {
			id := fmt.Sprintf("t%02d.c%03d", t, c)
			bp.Nodes = append(bp.Nodes, compiler.BlueprintNode{ID: id, Compute: "core.math.add@1"})
			bp.Edges = append(bp.Edges,
				compiler.BlueprintEdge{FromNode: prev, ToNode: id, FromPort: "out", ToPort: "a"},
				compiler.BlueprintEdge{FromNode: "lit.one", ToNode: id, FromPort: "out", ToPort: "b"},
			)
			prev = id
		}
		outID := fmt.Sprintf("t%02d.out", t)
		bp.Nodes = append(bp.Nodes, compiler.BlueprintNode{
			ID:      outID,
			Compute: "core.output@1",
			Config:  map[string]json.RawMessage{"name": json.RawMessage(fmt.Sprintf("%q", benchOutputPath(t)))},
		})
		bp.Edges = append(bp.Edges, compiler.BlueprintEdge{FromNode: prev, ToNode: outID, FromPort: "out", ToPort: "value"})
	}
	return bp
}

func benchInputPath(t int) string  { return fmt.Sprintf("bench.tower%02d.input", t) }
func benchOutputPath(t int) string { return fmt.Sprintf("bench.tower%02d.value", t) }

// compile20k runs the synthetic blueprint through the real compiler.
func compile20k(t testing.TB) *compiler.Graph {
	t.Helper()
	f := &benchFetcher{
		layout: &compiler.CanvasLayout{
			Version: "bench-v1",
			Root: compiler.LayoutNode{
				Kind:  "stack",
				ID:    "root",
				Props: map[string]json.RawMessage{"direction": json.RawMessage(`"vertical"`)},
			},
		},
		blueprint: gen20kBlueprint(),
	}
	start := time.Now()
	g, _, version, err := compiler.Compile(context.Background(), "bench-20k",
		compiler.PushEnvelope{CanvasVersion: "bench-v1", BlueBlueprintID: "bench-20k"}, f)
	if err != nil {
		t.Fatalf("20k blueprint failed to compile: %v", err)
	}
	t.Logf("compile: %d nodes in %v (version %s)", len(g.Nodes), time.Since(start), version)
	want := benchTowers*(benchChainAdds+2) + 1
	if len(g.Nodes) != want {
		t.Fatalf("compiled graph has %d nodes, want %d — the graph must never be truncated", len(g.Nodes), want)
	}
	return g
}

// TestPerf20k_CompileAndColdStartGate — ADR 003 §3.1.5: cold-start
// full evaluation of the 20 k scene ≤ 2 s. CI regression gate.
func TestPerf20k_CompileAndColdStartGate(t *testing.T) {
	skipUnderRace(t)
	g := compile20k(t)
	bundle := &compiler.RenderBundle{SceneVersion: g.SceneVersion}

	start := time.Now()
	scene := NewScene("bench-20k", g, bundle, NewComputeRegistry(), quietLogger())
	coldStart := time.Since(start)
	t.Logf("cold-start (load + full evaluation, %d nodes): %v", len(g.Nodes), coldStart)
	if coldStart > benchColdStartBudget {
		t.Fatalf("cold-start %v exceeds the %v budget (ADR 003 §3.1.5)", coldStart, benchColdStartBudget)
	}

	// The full pass must have evaluated EVERY tower's sink — a partial
	// walk (truncation) fails here. Each tower's chain at cold start is
	// 0 + 1×998 stages = 998.
	for tower := 0; tower < benchTowers; tower++ {
		raw, ok := scene.state.Get(benchOutputPath(tower))
		if !ok {
			t.Fatalf("tower %d sink absent after cold start — graph not fully evaluated", tower)
		}
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil || v != float64(benchChainAdds) {
			t.Fatalf("tower %d sink = %s, want %d", tower, raw, benchChainAdds)
		}
	}
}

// TestPerf20k_SingleInputDeltaP95Gate — ADR 003 §3.1.5: with the 20 k
// scene live, a single-input write whose dirty cone is ≤ 1 000 nodes
// reaches the subscriber as a delta with p95 ≤ 50 ms. CI regression
// gate.
func TestPerf20k_SingleInputDeltaP95Gate(t *testing.T) {
	skipUnderRace(t)
	g := compile20k(t)
	bundle := &compiler.RenderBundle{SceneVersion: g.SceneVersion}
	scene := NewScene("bench-20k", g, bundle, NewComputeRegistry(), quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(64)
	defer sub.Close()

	awaitDelta := func(tag string) *protocol.Delta {
		select {
		case msg := <-sub.Out:
			d, ok := msg.(*protocol.Delta)
			if !ok {
				t.Fatalf("%s: expected Delta, got %T", tag, msg)
			}
			return d
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no delta within 5s", tag)
			return nil
		}
	}

	write := func(tower, val int) {
		if !scene.Input(InputMsg{
			Path:   benchInputPath(tower),
			Value:  json.RawMessage(fmt.Sprintf("%d", val)),
			Source: "operator:bench",
		}) {
			t.Fatal("inbox full")
		}
	}

	// Warm-up: one write per tower (first write also seeds the input
	// leaf, which was absent at cold start).
	for tower := 0; tower < benchTowers; tower++ {
		write(tower, 1)
		awaitDelta("warm-up")
	}

	latencies := make([]time.Duration, 0, benchSamples)
	for i := 0; i < benchSamples; i++ {
		tower := i % benchTowers
		start := time.Now()
		write(tower, i+2)
		d := awaitDelta("sample")
		latencies = append(latencies, time.Since(start))

		// Cone integrity: the single write dirties its OWN tower's full
		// chain and nothing else — input leaf + 998 intermediates + 1
		// sink = 1 000 patches (≤ the 1 000-node cone budget + the input
		// leaf itself). Fewer ⇒ the cone was truncated (doctrine breach);
		// more ⇒ the walk leaked outside the cone.
		wantPatches := 1 + benchChainAdds + 1
		if len(d.Patches) != wantPatches {
			t.Fatalf("sample %d: delta carries %d patches, want %d (cone must be exact: no truncation, no leak)",
				i, len(d.Patches), wantPatches)
		}
		if benchChainAdds+1 > benchConeBudget {
			t.Fatalf("bench misconfigured: cone %d exceeds the %d budget", benchChainAdds+1, benchConeBudget)
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[len(latencies)*50/100]
	p95 := latencies[len(latencies)*95/100]
	worst := latencies[len(latencies)-1]
	t.Logf("single-input→delta over %d samples (cone %d nodes): p50=%v p95=%v max=%v",
		benchSamples, benchChainAdds+1, p50, p95, worst)
	if p95 > benchP95Budget {
		t.Fatalf("p95 %v exceeds the %v budget (ADR 003 §3.1.5)", p95, benchP95Budget)
	}
}
