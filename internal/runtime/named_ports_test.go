package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// stubFetcher is the minimal compiler.Fetcher the runtime named-port
// tests need to drive the REAL compiler (so the artefact under test is
// exactly what a push produces, not a hand-built graph).
type stubFetcher struct {
	layout    *compiler.CanvasLayout
	blueprint *compiler.BlueprintGraph
	manifest  compiler.ComputeManifest
}

func (f *stubFetcher) FetchCanvasLayout(context.Context, string) (*compiler.CanvasLayout, error) {
	return f.layout, nil
}
func (f *stubFetcher) FetchBlueprint(context.Context, string) (*compiler.BlueprintGraph, error) {
	return f.blueprint, nil
}
func (f *stubFetcher) FetchBlueprintGraph(context.Context, string, int) (*compiler.ResolvedBlueprintGraph, error) {
	return nil, errors.New("no blueprint references in this test")
}
func (f *stubFetcher) FetchComponent(context.Context, compiler.ComponentRef) (*compiler.UserComponent, error) {
	return nil, errors.New("no components in this test")
}
func (f *stubFetcher) FetchComputeManifest(context.Context) (compiler.ComputeManifest, error) {
	return f.manifest, nil
}

// TestScene_NamedPorts_ShuffledEdgeOrderSelect is ADR 003 resolution
// criterion 3's named-port clause (issue #79): a `core.flow.select@1`
// whose edges are authored in SHUFFLED order (when_false, condition,
// when_true) must still select correctly, because the artefact carries
// each edge's to_port and gatherInputs delivers values under those
// declared names. Under the pre-#79 positional wiring this exact graph
// broke: the shuffled first edge landed under `a`, selectFn read it as
// `condition` (a string, not a bool) and the compute errored — the sink
// leaf never moved. Passing here proves the per-compute name-fallback
// chains in compute.go are no longer load-bearing for compiled graphs.
func TestScene_NamedPorts_ShuffledEdgeOrderSelect(t *testing.T) {
	bp := &compiler.BlueprintGraph{
		ID: "bp-select",
		Nodes: []compiler.BlueprintNode{
			{ID: "in.cond", Compute: "core.input@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"flags.cond"`)}},
			{ID: "lit.t", Compute: "core.literal@1",
				Config: map[string]json.RawMessage{"value": json.RawMessage(`"ALPHA"`)}},
			{ID: "lit.f", Compute: "core.literal@1",
				Config: map[string]json.RawMessage{"value": json.RawMessage(`"BETA"`)}},
			{ID: "sel", Compute: "core.flow.select@1"},
			{ID: "out", Compute: "core.output@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"display.choice"`)}},
		},
		// Deliberately shuffled: when_false first, condition second,
		// when_true last. Positional zip would wire condition←"BETA".
		Edges: []compiler.BlueprintEdge{
			{FromNode: "lit.f", FromPort: "value", ToNode: "sel", ToPort: "when_false"},
			{FromNode: "in.cond", FromPort: "value", ToNode: "sel", ToPort: "condition"},
			{FromNode: "lit.t", FromPort: "value", ToNode: "sel", ToPort: "when_true"},
			{FromNode: "sel", FromPort: "result", ToNode: "out", ToPort: "value"},
		},
	}
	f := &stubFetcher{
		layout: &compiler.CanvasLayout{
			Version: "v1",
			Root:    compiler.LayoutNode{Kind: "stack", ID: "root"},
		},
		blueprint: bp,
		manifest: compiler.ComputeManifest{
			"core.input@1":       {IsPure: true, IsBounded: true, Version: "1"},
			"core.literal@1":     {IsPure: true, IsBounded: true, Version: "1"},
			"core.output@1":      {IsPure: true, IsBounded: true, Version: "1"},
			"core.flow.select@1": {IsPure: true, IsBounded: true, Version: "1"},
		},
	}
	graph, bundle, _, err := compiler.Compile(context.Background(), "scene-select",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-select"}, f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	scene := NewScene("scene-select", graph, bundle, NewComputeRegistry(), quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	// Cold start: condition leaf unseeded → readBool defaults to false →
	// the select must already resolve to when_false ("BETA"), not to a
	// positionally-misdelivered value.
	sub, snap := scene.Subscribe(16)
	if got := string(snap.State["display.choice"]); got != `"BETA"` {
		t.Fatalf(`cold-start display.choice = %s, want "BETA" (when_false via named port)`, got)
	}

	// condition=true → when_true ("ALPHA") must come through, proving
	// `condition` was wired by NAME despite arriving second in edge order.
	if !scene.Input(InputMsg{Path: "flags.cond", Value: json.RawMessage(`true`), Source: "test"}) {
		t.Fatal("inbox full?")
	}
	waitForLeaf(t, sub, "display.choice", `"ALPHA"`)

	// And back: condition=false → when_false ("BETA").
	if !scene.Input(InputMsg{Path: "flags.cond", Value: json.RawMessage(`false`), Source: "test"}) {
		t.Fatal("inbox full?")
	}
	waitForLeaf(t, sub, "display.choice", `"BETA"`)
}

// TestScene_NamedPorts_PositionalFallbackForLegacyArtefact pins the
// pre-#79 contract: a persisted artefact WITHOUT GraphNode.Inputs (as
// every pushed_versions row compiled before this change is stored)
// still wires positionally — upstreams land under `a`,`b`,`c` in edge
// order and selectFn's fallback chain resolves them. Such artefacts
// re-mint named wiring at their next push; until then nothing breaks.
func TestScene_NamedPorts_PositionalFallbackForLegacyArtefact(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "scene-legacy",
		SceneVersion: "sha256:legacy",
		Nodes: []compiler.GraphNode{
			{ID: "in.cond", Kind: "input", Path: "flags.cond", Compute: "core.input@1"},
			{ID: "lit.t", Kind: "input", Path: "lit.t", Compute: "core.literal@1"},
			{ID: "lit.f", Kind: "input", Path: "lit.f", Compute: "core.literal@1"},
			// Pre-#79 shape: Upstream only, in the conventional order
			// condition,when_true,when_false → positional a,b,c.
			{ID: "sel", Kind: "computed", Compute: "core.flow.select@1",
				Upstream: []string{"in.cond", "lit.t", "lit.f"}},
			{ID: "out", Kind: "output", Path: "display.choice", Compute: "core.output@1",
				Upstream: []string{"sel"}},
		},
		Defaults: map[string]json.RawMessage{
			"flags.cond": json.RawMessage(`true`),
			"lit.t":      json.RawMessage(`"ALPHA"`),
			"lit.f":      json.RawMessage(`"BETA"`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:legacy"}
	scene := NewScene("scene-legacy", graph, bundle, NewComputeRegistry(), quietLogger())

	_, snap := scene.Subscribe(8)
	if got := string(snap.State["display.choice"]); got != `"ALPHA"` {
		t.Fatalf(`legacy positional fallback broke: display.choice = %s, want "ALPHA"`, got)
	}
}

// TestScene_NamedPorts_ArithmeticShuffledEdges validates that a math node
// whose edges are authored in REVERSED order (subtrahend-first, minuend-
// second) still produces the correct result when named ports are carried.
// Without #79 the edges zip positionally: `b`←10 as port `a` and `a`←20
// as port `b`, so 10-20 = -10 (wrong). With named ports each value lands
// under its declared name — `arithmetic` reads `x`(=`a`) then `y`(=`b`),
// delivering 20-10 = 10 (correct). This is the non-select shuffle proof.
func TestScene_NamedPorts_ArithmeticShuffledEdges(t *testing.T) {
	bp := &compiler.BlueprintGraph{
		ID: "bp-arith",
		Nodes: []compiler.BlueprintNode{
			{ID: "lit.a", Compute: "core.literal@1",
				Config: map[string]json.RawMessage{"value": json.RawMessage(`20`)}},
			{ID: "lit.b", Compute: "core.literal@1",
				Config: map[string]json.RawMessage{"value": json.RawMessage(`10`)}},
			{ID: "sub", Compute: "core.math.sub@1"},
			{ID: "out", Compute: "core.output@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"result"`)}},
		},
		// Deliberately reversed: b-edge (y/second operand) listed first.
		// Positional zip → port `a`=10, port `b`=20 → 10-20 = -10 (wrong).
		// Named ports → port `x`=20, port `y`=10 → 20-10 = 10 (correct).
		Edges: []compiler.BlueprintEdge{
			{FromNode: "lit.b", FromPort: "value", ToNode: "sub", ToPort: "y"},
			{FromNode: "lit.a", FromPort: "value", ToNode: "sub", ToPort: "x"},
			{FromNode: "sub", FromPort: "result", ToNode: "out", ToPort: "value"},
		},
	}
	f := &stubFetcher{
		layout: &compiler.CanvasLayout{
			Version: "v1",
			Root:    compiler.LayoutNode{Kind: "stack", ID: "root"},
		},
		blueprint: bp,
		manifest: compiler.ComputeManifest{
			"core.literal@1":   {IsPure: true, IsBounded: true, Version: "1"},
			"core.output@1":    {IsPure: true, IsBounded: true, Version: "1"},
			"core.math.sub@1":  {IsPure: true, IsBounded: true, Version: "1"},
		},
	}
	graph, bundle, _, err := compiler.Compile(context.Background(), "scene-arith",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-arith"}, f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	scene := NewScene("scene-arith", graph, bundle, NewComputeRegistry(), quietLogger())
	_, snap := scene.Subscribe(8)
	if got := string(snap.State["result"]); got != `10` {
		t.Fatalf("arithmetic named-port shuffle: result = %s, want 10 (20-10); positional would give -10", got)
	}
}

// TestScene_NamedPorts_MixedScene exercises a graph where SOME nodes carry
// Inputs (compiled after #79) and others are hand-spliced without (legacy).
// Both must function side-by-side: the named-port path must not corrupt the
// positional path, and vice versa. This is the "partial migration" state
// that can arise when a scene mixes a freshly-pushed blueprint with a
// hand-crafted test fixture.
func TestScene_NamedPorts_MixedScene(t *testing.T) {
	// Legacy node: positional upstream, no Inputs.
	// Named node: carries Inputs (Upstream zipped 1:1).
	graph := &compiler.Graph{
		SceneID:      "scene-mixed",
		SceneVersion: "sha256:mixed",
		Nodes: []compiler.GraphNode{
			{ID: "in.score", Kind: "input", Path: "score.raw"},
			// Legacy output: no Inputs field, positional upstream.
			{ID: "out.legacy", Kind: "output", Path: "score.display",
				Compute:  "core.output@1",
				Upstream: []string{"in.score"}},
			{ID: "in.x", Kind: "input", Path: "math.x"},
			{ID: "in.y", Kind: "input", Path: "math.y"},
			// Named-port node: carries Inputs.
			{ID: "sum", Kind: "computed", Compute: "core.math.add@1",
				Upstream: []string{"in.x", "in.y"},
				Inputs: []compiler.GraphInput{
					{From: "in.x", Port: "x"},
					{From: "in.y", Port: "y"},
				}},
			{ID: "out.sum", Kind: "output", Path: "score.sum",
				Compute:  "core.output@1",
				Upstream: []string{"sum"},
				Inputs:   []compiler.GraphInput{{From: "sum", Port: "value"}}},
		},
		Defaults: map[string]json.RawMessage{
			"score.raw":     json.RawMessage(`7`),
			"score.display": json.RawMessage(`0`),
			"math.x":        json.RawMessage(`3`),
			"math.y":        json.RawMessage(`4`),
			"score.sum":     json.RawMessage(`0`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:mixed"}
	scene := NewScene("scene-mixed", graph, bundle, NewComputeRegistry(), quietLogger())
	_, snap := scene.Subscribe(8)

	if got := string(snap.State["score.display"]); got != `7` {
		t.Fatalf("legacy passthrough: score.display = %s, want 7", got)
	}
	if got := string(snap.State["score.sum"]); got != `7` {
		t.Fatalf("named-port add: score.sum = %s, want 7 (3+4)", got)
	}
}

// TestScene_NamedPorts_EmptyToPort validates the fail-open doctrine for
// malformed authoring: a GraphInput with an empty Port name must NOT drop
// the value. gatherInputs falls back to the positional name (`a` for index
// 0) so the value is still delivered — the compute may resolve it or not,
// but the value is never silently lost (doctrine: no value dropped).
func TestScene_NamedPorts_EmptyToPort(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "scene-empty-port",
		SceneVersion: "sha256:emptyport",
		Nodes: []compiler.GraphNode{
			{ID: "in.v", Kind: "input", Path: "raw.value"},
			// Inputs carries an empty Port — simulates malformed artefact.
			{ID: "out.v", Kind: "output", Path: "cooked.value",
				Compute:  "core.output@1",
				Upstream: []string{"in.v"},
				Inputs:   []compiler.GraphInput{{From: "in.v", Port: ""}}},
		},
		Defaults: map[string]json.RawMessage{
			"raw.value":    json.RawMessage(`42`),
			"cooked.value": json.RawMessage(`0`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:emptyport"}
	scene := NewScene("scene-empty-port", graph, bundle, NewComputeRegistry(), quietLogger())
	_, snap := scene.Subscribe(8)

	if got := string(snap.State["cooked.value"]); got != `42` {
		t.Fatalf("empty to_port: cooked.value = %s, want 42 (value must not be dropped)", got)
	}
}

// TestScene_NamedPorts_MissingUpstreamNotInState checks that when a named-
// port upstream is not yet seeded in state, gatherInputs silently omits
// it (the port is absent from the compute map) and the compute still
// runs to a defined result — here passthrough falls to `null`, not to
// a panic or a dropped compute cycle. This is the null/missing-input case.
func TestScene_NamedPorts_MissingUpstreamNotInState(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "scene-missing-up",
		SceneVersion: "sha256:missingup",
		Nodes: []compiler.GraphNode{
			// in.v is deliberately absent from Defaults — it has no state.
			{ID: "in.v", Kind: "input", Path: "raw.value"},
			{ID: "out.v", Kind: "output", Path: "cooked.value",
				Compute:  "core.output@1",
				Upstream: []string{"in.v"},
				Inputs:   []compiler.GraphInput{{From: "in.v", Port: "value"}}},
		},
		Defaults: map[string]json.RawMessage{
			// raw.value intentionally absent — upstream not in state.
			"cooked.value": json.RawMessage(`99`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:missingup"}
	scene := NewScene("scene-missing-up", graph, bundle, NewComputeRegistry(), quietLogger())
	_, snap := scene.Subscribe(8)

	// passthrough with an empty inputs map → returns json `null`.
	// The leaf must be `null`, never 99 (stale default), and the process
	// must not have panicked.
	got := string(snap.State["cooked.value"])
	if got != `null` {
		t.Fatalf("missing upstream: cooked.value = %s, want null (empty inputs → passthrough null)", got)
	}
}

// TestScene_NamedPorts_DirtyCheckTriggersRecompute verifies that when the
// upstream of a NAMED-PORT node becomes dirty, recompute fires for that
// node. Concretely: after cold start the sum is 3+4=7; we push x=10 and
// assert the sum leaf changes to 14. This pins that the dirty-check path
// in `recompute` (which uses ce.upstream, rebuilt from Inputs[i].From in
// NewScene) correctly tracks named-port upstreams.
func TestScene_NamedPorts_DirtyCheckTriggersRecompute(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "scene-dirty",
		SceneVersion: "sha256:dirty",
		Nodes: []compiler.GraphNode{
			{ID: "in.x", Kind: "input", Path: "val.x"},
			{ID: "in.y", Kind: "input", Path: "val.y"},
			{ID: "sum", Kind: "computed", Compute: "core.math.add@1",
				Upstream: []string{"in.x", "in.y"},
				Inputs: []compiler.GraphInput{
					{From: "in.x", Port: "x"},
					{From: "in.y", Port: "y"},
				}},
			{ID: "out.sum", Kind: "output", Path: "val.sum",
				Compute:  "core.output@1",
				Upstream: []string{"sum"},
				Inputs:   []compiler.GraphInput{{From: "sum", Port: "value"}}},
		},
		Defaults: map[string]json.RawMessage{
			"val.x":   json.RawMessage(`3`),
			"val.y":   json.RawMessage(`4`),
			"val.sum": json.RawMessage(`0`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:dirty"}
	scene := NewScene("scene-dirty", graph, bundle, NewComputeRegistry(), quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, snap := scene.Subscribe(16)
	if got := string(snap.State["val.sum"]); got != `7` {
		t.Fatalf("cold start: val.sum = %s, want 7", got)
	}

	// Update x → triggers dirty on in.x → recompute sum → val.sum = 14.
	if !scene.Input(InputMsg{Path: "val.x", Value: json.RawMessage(`10`), Source: "test"}) {
		t.Fatal("inbox full?")
	}
	waitForLeaf(t, sub, "val.sum", `14`)
}

// waitForLeaf drains deltas until the leaf carries want, or fails after
// a second.
func waitForLeaf(t *testing.T, sub *Subscription, leaf, want string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case msg := <-sub.Out:
			d, ok := msg.(*protocol.Delta)
			if !ok {
				continue
			}
			for _, p := range d.Patches {
				if p.Path == leaf && string(p.Value) == want {
					return
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s = %s", leaf, want)
		}
	}
}
