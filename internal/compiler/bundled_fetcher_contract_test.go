package compiler

import (
	"encoding/json"
	"testing"
)

// Contract lock for the embedded-local bundledFetcher (ADR 016 §B, issue
// #224). The bundledFetcher serves frozen artefacts in place of
// Canvas/Blue; it must return the SAME Go structs the compiler already
// decodes from the live HTTP fetch, so the compile path is unchanged. This
// test decodes representative frozen blobs through the real compiler structs
// — it is the executable target #224 builds to.
// See docs/contracts/embedded-local-contracts.md §B.

// TestBundledFetcher_CanvasLayoutShape locks FetchCanvasLayout's return
// shape: a CanvasLayout with version == the 64-hex key, a LayoutNode root,
// and opaque animations/assets carried verbatim (omitempty when absent).
func TestBundledFetcher_CanvasLayoutShape(t *testing.T) {
	const blob = `{
	  "version": "` + sixtyFourHex + `",
	  "root": {
	    "kind": "box",
	    "id": "root",
	    "props": {"style": {"width": 1600}},
	    "children": [
	      {"kind": "text", "id": "title", "bindings": {"value": "chat.last_message"}}
	    ]
	  },
	  "operator_inputs": [{"path": "score", "label": "Score", "type": "number"}],
	  "assets": {"allowedHosts": ["cdn.cyell.dev"], "fonts": [], "preload": []}
	}`
	var l CanvasLayout
	if err := json.Unmarshal([]byte(blob), &l); err != nil {
		t.Fatal(err)
	}
	if l.Version != sixtyFourHex {
		t.Fatalf("version = %q", l.Version)
	}
	if l.Root.Kind != "box" || len(l.Root.Children) != 1 || l.Root.Children[0].Kind != "text" {
		t.Fatalf("root tree not decoded: %+v", l.Root)
	}
	if l.Root.Children[0].Bindings["value"] != "chat.last_message" {
		t.Fatalf("binding lost: %+v", l.Root.Children[0].Bindings)
	}
	// assets must survive as opaque bytes (feeds the Solar host allowlist).
	if len(l.Assets) == 0 {
		t.Fatal("assets block must be carried verbatim")
	}
	// render-bundle-only fields are NEVER on a fetched layout.
	if l.Root.Keyframes != nil || l.Root.AnimateInitial != nil {
		t.Fatal("fetched layout must not carry lowered render-bundle fields")
	}
}

// TestBundledFetcher_BlueprintGraphShape locks FetchBlueprint's collapsed
// return: a flat BlueprintGraph {id,nodes,edges,variables}, where a node
// carries its compute on the `definition` wire field and edges use
// snake_case from_node/to_node.
func TestBundledFetcher_BlueprintGraphShape(t *testing.T) {
	const blob = `{
	  "id": "11111111-1111-1111-1111-111111111111",
	  "nodes": [
	    {"id": "n1", "definition": "core.output@1", "config": {"name": "title"}},
	    {"id": "n2", "definition": "user.score-to-color@3",
	     "reference": {"blueprint_id": "22222222-2222-2222-2222-222222222222", "version": 3}}
	  ],
	  "edges": [{"from_node": "n2", "from_port": "out", "to_node": "n1", "to_port": "in"}],
	  "variables": [{"id": "v1", "name": "palette", "type": "list", "value": ["#fff"]}]
	}`
	var g BlueprintGraph
	if err := json.Unmarshal([]byte(blob), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 2 || g.Nodes[0].Compute != "core.output@1" {
		t.Fatalf("definition must decode into Compute: %+v", g.Nodes)
	}
	if g.Nodes[1].Reference == nil || g.Nodes[1].Reference.Version != 3 {
		t.Fatalf("reference node not decoded: %+v", g.Nodes[1])
	}
	if len(g.Edges) != 1 || g.Edges[0].FromNode != "n2" || g.Edges[0].ToNode != "n1" {
		t.Fatalf("snake_case edge not decoded: %+v", g.Edges)
	}
	if len(g.Variables) != 1 || g.Variables[0].Name != "palette" {
		t.Fatalf("variables not decoded: %+v", g.Variables)
	}
}

// TestBundledFetcher_ResolvedGraphShape locks FetchBlueprintGraph's pinned
// return: ResolvedBlueprintGraph with interface pins (data|exec) and the
// served purity stamped verbatim (never recomputed by Orion).
func TestBundledFetcher_ResolvedGraphShape(t *testing.T) {
	const blob = `{
	  "blueprint_id": "22222222-2222-2222-2222-222222222222",
	  "version": 3,
	  "nodes": [{"id": "in", "definition": "core.input@1", "config": {"name": "score"}}],
	  "edges": [],
	  "variables": [],
	  "interface": {
	    "inputs":  [{"name": "score", "type": "number", "required": true},
	                {"name": "exec_in", "type": "exec", "kind": "exec", "required": false}],
	    "outputs": [{"name": "color", "type": "string", "required": true}]
	  },
	  "purity": {"is_pure": true, "is_bounded": true},
	  "status": "published"
	}`
	var r ResolvedBlueprintGraph
	if err := json.Unmarshal([]byte(blob), &r); err != nil {
		t.Fatal(err)
	}
	if r.Version != 3 {
		t.Fatalf("version = %d", r.Version)
	}
	if len(r.Interface.Inputs) != 2 || r.Interface.Inputs[1].Kind != "exec" {
		t.Fatalf("exec pin discriminator lost: %+v", r.Interface.Inputs)
	}
	if !r.Purity.IsPure || !r.Purity.IsBounded {
		t.Fatalf("served purity must decode verbatim: %+v", r.Purity)
	}
	// Blue serves an extra `status`; Orion ignores it (must not break decode).
}

// sixtyFourHex is a valid 64-char lowercase-hex content address (the bundle
// key shape FetchCanvasLayout is called with).
const sixtyFourHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
