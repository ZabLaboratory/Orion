package compiler

import (
	"context"
	"encoding/json"
	"testing"
)

// Forge regression for ADR 016 RC-6 (e2e #152): a REFERENCE-FREE top-level
// blueprint that declares its OWN `variables[].value` CONSTANT (e.g. a
// `palette` colour list read by score-to-color) never runs the reference
// expander, so before this fix its declared `palette` value was never folded
// into graph.Defaults → the `__vars..palette` leaf stayed empty at runtime →
// `core.variable.get@1`=null → `list-at(null)`=null → the per-player
// `pl.*.color` leaves stuck at the COLD_COLOR placeholder (10/40 leaves null).
//
// The fix folds a top-level blueprint's declared constants on the PUSH path via
// the SAME foldDeclaredVariables helper the in-body simulate path uses.

// topVarPalette is the colour-list constant a top-level blueprint declares.
var topVarPalette = json.RawMessage(`["#1f6feb","#2ea043","#d29922","#da3633"]`)

// topLevelPaletteFetcher serves a reference-free blueprint whose own
// `variables[]` declares the `palette` CONSTANT, read by a variable.get feeding
// a list-at whose element is written to the `pl.0.color` output leaf — the live
// score-to-color cone shape, but authored top-level (no `reference` node).
func topLevelPaletteFetcher(vars []BlueprintVariable) *fakeFetcher {
	bp := &BlueprintGraph{
		ID: "bp-stc",
		Nodes: []BlueprintNode{
			{ID: "score", Compute: coreLiteral,
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`2`)},
				Outputs: []BlueprintPort{dataOut("value")}},
			{ID: "palette", Compute: coreVariableGet,
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"palette"`)},
				Outputs: []BlueprintPort{dataOut("value")}},
			{ID: "lookup", Compute: "core.data.list-at@1",
				Inputs:  []BlueprintPort{dataIn("list"), dataIn("index")},
				Outputs: []BlueprintPort{dataOut("element")}},
			outputNode("slotColor", "pl.0.color"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "palette", FromPort: "value", ToNode: "lookup", ToPort: "list"},
			{FromNode: "score", FromPort: "value", ToNode: "lookup", ToPort: "index"},
			{FromNode: "lookup", FromPort: "element", ToNode: "slotColor", ToPort: "value"},
		},
		Variables: vars,
	}
	m := pureManifest()
	m["core.variable.get@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	m["core.data.list-at@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	return &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-stc": bp},
		components: map[ComponentRef]*UserComponent{},
		manifest:   m,
	}
}

// TestCompile_TopLevelVariable_ConstantFolded is RC-6: a top-level
// reference-free blueprint's declared `palette` CONSTANT must reach
// graph.Defaults at the `__vars..palette` leaf its variable.get reads — proving
// score-to-color sees the real palette, not null.
func TestCompile_TopLevelVariable_ConstantFolded(t *testing.T) {
	f := topLevelPaletteFetcher([]BlueprintVariable{
		{ID: "v_palette", Name: "palette", Type: "core.primitive.json", Value: topVarPalette},
	})
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-stc"}, f)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	// Legacy empty key → the leaf is the empty-key form `__vars..palette`.
	leaf := varsLeaf("palette") // "__vars..palette"
	seed, ok := g.Defaults[leaf]
	if !ok {
		// Pre-fix RED: the top-level constant was dropped → leaf absent → null.
		t.Fatalf("no default seeded at %q — the top-level `palette` constant was DROPPED (e2e #152 bug)\nDefaults=%v", leaf, g.Defaults)
	}
	if !equalJSON(t, seed, topVarPalette) {
		t.Fatalf("seed at %q = %s, want palette %s", leaf, seed, topVarPalette)
	}
}

// TestCompile_TopLevelVariable_ValuelessNotFolded guards the reseed invariant
// (ADR 003/006): a value-less variable is pure MUTABLE shared state, reseeded
// from declared defaults on activation — it must NOT be folded as a compile
// constant, else a runtime-mutable slot would be frozen to an empty seed.
func TestCompile_TopLevelVariable_ValuelessNotFolded(t *testing.T) {
	f := topLevelPaletteFetcher([]BlueprintVariable{
		{ID: "v_palette", Name: "palette", Type: "core.primitive.json"}, // no Value
	})
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-stc"}, f)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	if _, ok := g.Defaults[varsLeaf("palette")]; ok {
		t.Fatalf("value-less variable was folded as a constant — violates the reseed invariant (ADR 003/006)")
	}
}
