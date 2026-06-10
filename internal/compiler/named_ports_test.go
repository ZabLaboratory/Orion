package compiler

import (
	"context"
	"encoding/json"
	"testing"
)

// Issue #79 (ADR 003 §3.1.1, phase 0): the edges' to_port names are
// carried into the compiled graph artefact, zipped 1:1 with Upstream.
func TestCompile_CarriesNamedPorts(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{ID: "in.a", Compute: "core.input@1"},
			{ID: "in.b", Compute: "core.input@1"},
			outputNode("out.sum", "score.team_a"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "in.a", ToNode: "out.sum", FromPort: "out", ToPort: "x"},
			{FromNode: "in.b", ToNode: "out.sum", FromPort: "out", ToPort: "y"},
		},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		manifest:   pureManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	sink, ok := graphNodeByPath(g, "score.team_a")
	if !ok {
		t.Fatalf("sink node missing; nodes = %+v", g.Nodes)
	}
	want := []GraphInput{
		{From: "in.a", Port: "x"},
		{From: "in.b", Port: "y"},
	}
	if len(sink.Inputs) != len(want) {
		t.Fatalf("sink.Inputs = %+v, want %+v", sink.Inputs, want)
	}
	for i := range want {
		if sink.Inputs[i] != want[i] {
			t.Fatalf("sink.Inputs[%d] = %+v, want %+v", i, sink.Inputs[i], want[i])
		}
	}
	// Zipped 1:1 with Upstream — same edges, same order.
	if len(sink.Upstream) != len(sink.Inputs) {
		t.Fatalf("Upstream (%d) and Inputs (%d) not zipped", len(sink.Upstream), len(sink.Inputs))
	}
	for i := range sink.Inputs {
		if sink.Upstream[i] != sink.Inputs[i].From {
			t.Fatalf("Upstream[%d]=%q != Inputs[%d].From=%q", i, sink.Upstream[i], i, sink.Inputs[i].From)
		}
	}
}

// In a multi-blueprint scene, Inputs[].From references sibling node ids —
// they take the same "<key>." prefix as Upstream; port names are NEVER
// prefixed (they address the node's own port set, not state paths).
func TestCompile_NamedPortsPrefixedPerBlueprint(t *testing.T) {
	f := &fakeFetcher{
		layouts: map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{
			"bp-score": scoreBlueprint("bp-score", "value"),
			"bp-timer": scoreBlueprint("bp-timer", "value"),
		},
		manifest: pureManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{
			{Key: "score", ID: "bp-score"},
			{Key: "timer", ID: "bp-timer"},
		}}, f)
	if err != nil {
		t.Fatalf("N-blueprint compile errored: %v", err)
	}
	sink, ok := graphNodeByPath(g, "score.value")
	if !ok {
		t.Fatalf("score.value sink missing; nodes = %+v", g.Nodes)
	}
	want := []GraphInput{
		{From: "score.in.a", Port: "x"},
		{From: "score.in.b", Port: "y"},
	}
	if len(sink.Inputs) != len(want) {
		t.Fatalf("sink.Inputs = %+v, want %+v", sink.Inputs, want)
	}
	for i := range want {
		if sink.Inputs[i] != want[i] {
			t.Fatalf("sink.Inputs[%d] = %+v, want %+v", i, sink.Inputs[i], want[i])
		}
	}
}

// Wire-stability both ways: a pre-#79 persisted artefact (no `inputs`
// key) deserialises with Inputs==nil — the runtime's positional fallback
// engages; a node compiled without inbound edges serialises WITHOUT an
// `inputs` key (omitempty), so edge-free graphs hash byte-identically to
// their pre-#79 form.
func TestGraphNode_NamedPortsWireCompat(t *testing.T) {
	const legacy = `{"id":"out","kind":"output","path":"score.team_a","compute":"core.output@1","upstream":["in.a"]}`
	var n GraphNode
	if err := json.Unmarshal([]byte(legacy), &n); err != nil {
		t.Fatalf("unmarshal pre-#79 node: %v", err)
	}
	if n.Inputs != nil {
		t.Fatalf("pre-#79 artefact decoded with Inputs = %+v, want nil (positional fallback)", n.Inputs)
	}

	raw, err := json.Marshal(GraphNode{ID: "in.a", Kind: "input"})
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatal(err)
	}
	if _, ok := asMap["inputs"]; ok {
		t.Fatalf("edge-free node serialised an inputs key: %s", raw)
	}
}
