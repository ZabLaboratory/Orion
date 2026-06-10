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
