package compiler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Forge regression tests for the `variables[].value` constant-seed drop
// (Orion #192, ADR 014 — extends the #190 per-instance var-namespacing series).
//
// A referenced DATA-ONLY function may declare a blueprint-local CONSTANT in its
// `variables[]` (Blue schemas/graph.py Variable, served verbatim by the
// graph-resolution endpoint). The body reads it through a
// `core.variable.get@1 {variable: <name>}` whose leaf is `__vars..<name>`.
// Nothing wires that leaf, so the constant reaches the runtime ONLY as a graph
// default. Before #192:
//   - ResolvedBlueprintGraph had no `Variables` field → Blue's `variables[]`
//     JSON was dropped on deserialisation (Go ignores unknown keys);
//   - even when read, no code seeded `variables[].value` into Defaults.
// Result: the `__vars..` leaf stayed null → variable.get=null → list-at(null)
// =null → the score-to-color colour slot fell to its COLD_COLOR guard on air.
//
// The fix harvests each inlined reference's valued variables into
// BlueprintGraph.Defaults, namespaced per reference INSTANCE with the SAME
// varNS the reading `core.variable.get@1` is rewritten with — so the seed leaf
// is byte-identical to the leaf the get binds to.
//
// These run directly against expandReferences so the assertion is on the flat
// graph + its harvested Defaults, independent of the downstream compile loop.

// paletteJSON is the static colour list score-to-color declares in
// `variables[].value` — the constant whose drop blanked the colour cone.
var paletteJSON = json.RawMessage(`["#1f6feb","#2ea043","#d29922","#da3633"]`)

// scoreToColorGraph mirrors Blue's `score-to-color` DATA-ONLY function colour
// cone (tests/fixtures/score_to_color.py): a `core.variable.get@1` reading the
// `palette` variable feeds `core.data.list-at@1`, whose element is written to
// the `color` output. The palette itself is a `variables[].value` CONSTANT —
// no node, no edge, no port carries it; only the variable seed does.
//
//	color ← out_color ← list-at(list=palette.get, index=in_score) ;
//	palette = variables[{name: "palette", value: [...]}]
func scoreToColorGraph(id string, version int) *ResolvedBlueprintGraph {
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			inputNode("in_score", "score"), // interface data pin
			{ID: "palette", Compute: coreVariableGet,
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"palette"`)},
				Outputs: []BlueprintPort{dataOut("value")}},
			{ID: "lookup", Compute: "core.data.list-at@1",
				Inputs:  []BlueprintPort{dataIn("list"), dataIn("index")},
				Outputs: []BlueprintPort{dataOut("element")}},
			outputNode("out_color", "color"), // interface data OUT pin
		},
		Edges: []BlueprintEdge{
			{FromNode: "palette", FromPort: "value", ToNode: "lookup", ToPort: "list"},
			{FromNode: "in_score", FromPort: "value", ToNode: "lookup", ToPort: "index"},
			{FromNode: "lookup", FromPort: "element", ToNode: "out_color", ToPort: "value"},
		},
		// The dropped-on-#192 payload: a CONSTANT variable carrying `value`.
		Variables: []BlueprintVariable{
			{ID: "v_palette", Name: "palette", Type: "core.primitive.json", Value: paletteJSON},
		},
		Interface: BlueprintInterface{
			Inputs: []BlueprintInterfacePin{
				{Name: "score", Type: "any", Kind: "data"},
			},
			Outputs: []BlueprintInterfacePin{
				{Name: "color", Type: "any", Kind: "data"},
			},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

func stcFetcher() *fakeFetcher {
	return &fakeFetcher{graphs: map[string]*ResolvedBlueprintGraph{
		"bp-stc@5": scoreToColorGraph("bp-stc", 5),
	}}
}

// callScoreToColor authors one data-only reference to score-to-color: the
// caller feeds `score` and reads `color`. on-start arms nothing in the body
// (data-only), so this is the live board's data-cone shape.
func callScoreToColor(callID string) *BlueprintGraph {
	return &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "src", Compute: coreLiteral,
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`2`)},
				Outputs: []BlueprintPort{dataOut("value")}},
			refNode(callID, "bp-stc", 5),
			outputNode("slotColor", "pl.0.color"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "src", FromPort: "value", ToNode: callID, ToPort: "score"},
			{FromNode: callID, FromPort: "color", ToNode: "slotColor", ToPort: "value"},
		},
	}
}

// readerLeaf returns the `__vars..` leaf the surviving inlined
// `core.variable.get@1` binds to (nodeLeafPath) — the address its seed MUST
// match. Fatal unless exactly one such node survived expansion.
func readerLeaf(t *testing.T, g *BlueprintGraph) string {
	t.Helper()
	var leaf string
	var n int
	for i := range g.Nodes {
		if g.Nodes[i].Compute != coreVariableGet {
			continue
		}
		n++
		leaf = nodeLeafPath(g.Nodes[i])
	}
	if n != 1 {
		t.Fatalf("want exactly 1 core.variable.get@1 after expansion, got %d", n)
	}
	return leaf
}

// TestExpand_VarSeed_MaterialisedPerInstance is RC #1: a data-only reference
// declaring a `variables[].value` constant must surface that value in
// BlueprintGraph.Defaults under the SAME per-instance `__vars..` leaf the
// inlined `core.variable.get@1` reads — proving the seed was harvested, not
// dropped, and is addressable by the runtime read.
func TestExpand_VarSeed_MaterialisedPerInstance(t *testing.T) {
	flat, diags := expandReferences(context.Background(), callScoreToColor("L0stc"), stcFetcher())
	if len(diags) > 0 {
		t.Fatalf("expandReferences returned diagnostics: %+v", diags)
	}

	leaf := readerLeaf(t, flat) // e.g. "__vars..bpref1_L0stc_palette"
	if !strings.HasPrefix(leaf, varsLeafPrefix) {
		t.Fatalf("variable.get leaf %q is not a __vars leaf", leaf)
	}

	// Pre-fix RED: Defaults nil / leaf absent → the seed was dropped.
	if flat.Defaults == nil {
		t.Fatal("BlueprintGraph.Defaults is nil — the `variables[].value` seed was DROPPED (the #192 bug)")
	}
	seed, ok := flat.Defaults[leaf]
	if !ok {
		t.Fatalf("no default seeded at the variable.get leaf %q\nDefaults=%v", leaf, flat.Defaults)
	}

	// GREEN: the seed is the declared palette, byte-for-byte.
	if !equalJSON(t, seed, paletteJSON) {
		t.Fatalf("seed at %q = %s, want palette %s", leaf, seed, paletteJSON)
	}

	// The leaf is per-instance namespaced (carries the call id), so 10 refs
	// own 10 distinct leaves rather than clobbering one shared `palette`.
	if !strings.Contains(leaf, "L0stc") {
		t.Fatalf("seed leaf %q is not namespaced by the reference instance", leaf)
	}
	if !strings.Contains(leaf, "palette") {
		t.Fatalf("seed leaf %q lost the original variable name", leaf)
	}
}

// TestExpand_VarSeed_DistinctPerInstance is the per-instance guard: two
// references to score-to-color must seed TWO distinct `__vars..` leaves, each
// carrying the palette — so two colour slots do not share (and clobber) one
// `palette` leaf.
func TestExpand_VarSeed_DistinctPerInstance(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "src", Compute: coreLiteral,
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`2`)},
				Outputs: []BlueprintPort{dataOut("value")}},
			refNode("L0stc", "bp-stc", 5),
			refNode("R4stc", "bp-stc", 5),
			outputNode("c0", "pl.0.color"),
			outputNode("c4", "pl.4.color"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "src", FromPort: "value", ToNode: "L0stc", ToPort: "score"},
			{FromNode: "src", FromPort: "value", ToNode: "R4stc", ToPort: "score"},
			{FromNode: "L0stc", FromPort: "color", ToNode: "c0", ToPort: "value"},
			{FromNode: "R4stc", FromPort: "color", ToNode: "c4", ToPort: "value"},
		},
	}
	flat, diags := expandReferences(context.Background(), bp, stcFetcher())
	if len(diags) > 0 {
		t.Fatalf("diagnostics: %+v", diags)
	}

	// Two distinct __vars seeds, both the palette.
	varSeeds := map[string]json.RawMessage{}
	for k, v := range flat.Defaults {
		if strings.HasPrefix(k, varsLeafPrefix) {
			varSeeds[k] = v
		}
	}
	if len(varSeeds) != 2 {
		t.Fatalf("want 2 distinct __vars palette seeds (one per instance), got %d: %v", len(varSeeds), varSeeds)
	}
	for leaf, seed := range varSeeds {
		if !equalJSON(t, seed, paletteJSON) {
			t.Fatalf("seed at %q = %s, want palette", leaf, seed)
		}
	}

	// Every inlined variable.get binds to one of the seeded leaves (read↔seed
	// matched per instance — no orphaned read, no shared leaf).
	gets := 0
	for i := range flat.Nodes {
		if flat.Nodes[i].Compute != coreVariableGet {
			continue
		}
		gets++
		leaf := nodeLeafPath(flat.Nodes[i])
		if _, ok := varSeeds[leaf]; !ok {
			t.Fatalf("variable.get leaf %q has no matching seed (read↔seed unmatched)", leaf)
		}
	}
	if gets != 2 {
		t.Fatalf("want 2 inlined variable.get nodes, got %d", gets)
	}
}

// TestExpand_VarSeed_NoVariables_Unchanged is the anti-regression: a reference
// with no `variables[]` (the #190 doubler) seeds NO __vars default — the carrier
// stays nil, byte-identical to pre-#192.
func TestExpand_VarSeed_NoVariables_Unchanged(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "src", Compute: coreLiteral,
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`3`)},
				Outputs: []BlueprintPort{dataOut("value")}},
			refNode("L0x", "bp-double", 3),
			outputNode("out", "doubled"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "src", FromPort: "value", ToNode: "L0x", ToPort: "x"},
			{FromNode: "L0x", FromPort: "result", ToNode: "out", ToPort: "value"},
		},
	}
	f := &fakeFetcher{graphs: map[string]*ResolvedBlueprintGraph{
		"bp-double@3": doublerGraph("bp-double", 3),
	}}
	flat, diags := expandReferences(context.Background(), bp, f)
	if len(diags) > 0 {
		t.Fatalf("diagnostics: %+v", diags)
	}
	for k := range flat.Defaults {
		if strings.HasPrefix(k, varsLeafPrefix) {
			t.Fatalf("a variable-free reference seeded a __vars default %q — regression", k)
		}
	}
}

// TestResolvedGraph_VariablesDeserialised locks root cause #1 (Orion #192): the
// `variables[]` array Blue serves must survive JSON deserialisation into
// ResolvedBlueprintGraph. Before the `Variables` field existed Go dropped the
// unknown key silently, so the palette never reached expandOne to be seeded.
func TestResolvedGraph_VariablesDeserialised(t *testing.T) {
	// The byte shape Blue's graph-resolution endpoint serves (schemas/version.py
	// ResolvedGraph + schemas/graph.py Variable).
	body := []byte(`{
		"blueprint_id": "bp-stc",
		"version": 5,
		"status": "published",
		"nodes": [],
		"edges": [],
		"variables": [
			{"id": "v_palette", "name": "palette", "type": "core.primitive.json",
			 "value": ["#1f6feb","#2ea043"]}
		],
		"interface": {"inputs": [], "outputs": []},
		"purity": {"is_pure": true, "is_bounded": true}
	}`)
	var g ResolvedBlueprintGraph
	if err := json.Unmarshal(body, &g); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(g.Variables) != 1 {
		t.Fatalf("variables[] dropped on deserialisation (the #192 root cause): got %d", len(g.Variables))
	}
	v := g.Variables[0]
	if v.Name != "palette" {
		t.Fatalf("variable name = %q, want palette", v.Name)
	}
	if !equalJSON(t, v.Value, json.RawMessage(`["#1f6feb","#2ea043"]`)) {
		t.Fatalf("variable value = %s, want the palette list", v.Value)
	}
}

func equalJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("unmarshal a=%s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("unmarshal b=%s: %v", b, err)
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(ax) == string(by)
}
