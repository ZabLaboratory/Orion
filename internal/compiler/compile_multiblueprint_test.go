package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// scoreBlueprint builds a two-input → one-output blueprint whose sink writes
// the leaf `value`. Distinct ids/leaf per call keep N-blueprint cases honest.
func scoreBlueprint(id, leaf string) *BlueprintGraph {
	return &BlueprintGraph{
		ID: id,
		Nodes: []BlueprintNode{
			{ID: "in.a", Compute: "core.input@1"},
			{ID: "in.b", Compute: "core.input@1"},
			outputNode("out.v", leaf),
		},
		Edges: []BlueprintEdge{
			{FromNode: "in.a", ToNode: "out.v", FromPort: "out", ToPort: "x"},
			{FromNode: "in.b", ToNode: "out.v", FromPort: "out", ToPort: "y"},
		},
	}
}

func graphNodeByPath(g *Graph, path string) (GraphNode, bool) {
	for _, n := range g.Nodes {
		if n.Path == path {
			return n, true
		}
	}
	return GraphNode{}, false
}

// Criterion 1 / R4 (THE non-regression): a legacy single-field push and the
// equivalent blueprints:[{key:"",id:X}] push compile to the BYTE-IDENTICAL
// scene_version. The empty key's empty prefix means leaf paths, node ids and
// defaults are unchanged from the pre-001 single-blueprint world — an old
// producer is unaffected.
func TestCompile_BackCompat_SingularEqualsEmptyKeyList(t *testing.T) {
	bp := scoreBlueprint("bp-1", "score.team_a")
	newFetcher := func() *fakeFetcher {
		return &fakeFetcher{
			layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
			blueprints: map[string]*BlueprintGraph{"bp-1": bp},
			manifest:   pureManifest(),
		}
	}

	gLegacy, _, vLegacy, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, newFetcher())
	if err != nil {
		t.Fatalf("legacy compile: %v", err)
	}
	gList, _, vList, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{{Key: "", ID: "bp-1"}}}, newFetcher())
	if err != nil {
		t.Fatalf("empty-key-list compile: %v", err)
	}

	if vLegacy != vList {
		t.Fatalf("back-compat broken: singular scene_version %q != empty-key-list %q", vLegacy, vList)
	}
	// R4 byte-identity at the path level: the leaf stays unprefixed.
	if _, ok := graphNodeByPath(gLegacy, "score.team_a"); !ok {
		t.Fatalf("legacy leaf path drifted; nodes = %+v", gLegacy.Nodes)
	}
	if _, ok := graphNodeByPath(gList, "score.team_a"); !ok {
		t.Fatalf("empty-key list re-prefixed the legacy leaf; nodes = %+v", gList.Nodes)
	}
	// And node ids must NOT be prefixed for the empty key.
	for _, n := range gList.Nodes {
		if n.ID == ".in.a" || n.ID == ".out.v" {
			t.Fatalf("empty key prefixed a node id with a leading dot: %q", n.ID)
		}
	}
}

// Criterion 3: an N-blueprint push (≥2 distinct ids/keys) compiles with no
// error, and each blueprint's leaves appear under its <key>. prefix.
func TestCompile_NBlueprints_LeafPathsPrefixed(t *testing.T) {
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
	// Both blueprints declare leaf `value`; without prefixing they would
	// collide. The prefix splits them into score.value and timer.value.
	if _, ok := graphNodeByPath(g, "score.value"); !ok {
		t.Fatalf("missing score.value leaf; nodes = %+v", g.Nodes)
	}
	if _, ok := graphNodeByPath(g, "timer.value"); !ok {
		t.Fatalf("missing timer.value leaf; nodes = %+v", g.Nodes)
	}
	if _, ok := graphNodeByPath(g, "value"); ok {
		t.Fatal("unprefixed leaf `value` present — namespacing did not engage")
	}
	// Node ids are namespaced too, so the two `out.v` sinks don't collapse.
	ids := make(map[string]int)
	for _, n := range g.Nodes {
		ids[n.ID]++
	}
	if ids["score.out.v"] != 1 || ids["timer.out.v"] != 1 {
		t.Fatalf("blueprint node ids not namespaced as expected: %+v", ids)
	}
}

// Criterion 5: re-ordering blueprints[] in the request yields the SAME
// scene_version (sort-before-hash, §3.5).
func TestCompile_NBlueprints_OrderIndependentHash(t *testing.T) {
	newFetcher := func() *fakeFetcher {
		return &fakeFetcher{
			layouts: map[string]*CanvasLayout{"v1": minimalLayout("v1")},
			blueprints: map[string]*BlueprintGraph{
				"bp-a": scoreBlueprint("bp-a", "a_leaf"),
				"bp-b": scoreBlueprint("bp-b", "b_leaf"),
				"bp-c": scoreBlueprint("bp-c", "c_leaf"),
			},
			manifest: pureManifest(),
		}
	}
	order1 := []BlueprintRef{{Key: "a", ID: "bp-a"}, {Key: "b", ID: "bp-b"}, {Key: "c", ID: "bp-c"}}
	order2 := []BlueprintRef{{Key: "c", ID: "bp-c"}, {Key: "a", ID: "bp-a"}, {Key: "b", ID: "bp-b"}}

	_, _, v1, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: order1}, newFetcher())
	if err != nil {
		t.Fatal(err)
	}
	_, _, v2, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: order2}, newFetcher())
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Fatalf("hash depends on authored order: %s vs %s", v1, v2)
	}
}

// Criterion 2: both blue_blueprint_id and blueprints[] set → conflict. The
// 400 itself is asserted at the API layer; here we assert the normaliser (the
// single source of truth) flags it.
func TestNormalizeBlueprints_Conflict(t *testing.T) {
	_, err := NormalizeBlueprints(PushEnvelope{
		BlueBlueprintID: "bp-1",
		Blueprints:      []BlueprintRef{{Key: "score", ID: "bp-2"}},
	})
	if !errors.Is(err, ErrEnvelopeBlueprintConflict) {
		t.Fatalf("want ErrEnvelopeBlueprintConflict, got %v", err)
	}
}

// NormalizeBlueprints back-compat table (§3.1), the cases not covered above.
func TestNormalizeBlueprints_Table(t *testing.T) {
	cases := []struct {
		name string
		env  PushEnvelope
		want []BlueprintRef
	}{
		{"empty → blueprint-free", PushEnvelope{}, nil},
		{"none sentinel → blueprint-free", PushEnvelope{BlueBlueprintID: "none"}, nil},
		{"legacy single → key-empty list", PushEnvelope{BlueBlueprintID: "bp-1"}, []BlueprintRef{{Key: "", ID: "bp-1"}}},
		{
			"plural sorted by key",
			PushEnvelope{Blueprints: []BlueprintRef{{Key: "z", ID: "bz"}, {Key: "a", ID: "ba"}}},
			[]BlueprintRef{{Key: "a", ID: "ba"}, {Key: "z", ID: "bz"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeBlueprints(tc.env)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Duplicate keys within an envelope are rejected (§3.1/§5 R1).
func TestCompile_DuplicateBlueprintKey(t *testing.T) {
	f := &fakeFetcher{
		layouts: map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{
			"bp-a": scoreBlueprint("bp-a", "x"),
			"bp-b": scoreBlueprint("bp-b", "y"),
		},
		manifest: pureManifest(),
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{
			{Key: "dup", ID: "bp-a"},
			{Key: "dup", ID: "bp-b"},
		}}, f)
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrDuplicateBlueprintKey) {
		t.Fatalf("want DUPLICATE_BLUEPRINT_KEY, got %v", err)
	}
}

// Criterion 4 (negative): a component binding referencing an undeclared
// blueprint key fails with UNKNOWN_BLUEPRINT_KEY.
func TestCompile_UnknownBlueprintKey(t *testing.T) {
	layout := &CanvasLayout{
		Version: "v1",
		Root: LayoutNode{
			Kind: "stack",
			ID:   "root",
			Children: []LayoutNode{{
				Kind:     "text",
				ID:       "label",
				Bindings: map[string]string{"value": "typo.value"}, // typo: key "typo" undeclared
			}},
		},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": layout},
		blueprints: map[string]*BlueprintGraph{"bp-score": scoreBlueprint("bp-score", "value")},
		manifest:   pureManifest(),
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{{Key: "score", ID: "bp-score"}}}, f)
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrUnknownBlueprintKey) {
		t.Fatalf("want UNKNOWN_BLUEPRINT_KEY, got %v", err)
	}
}

// Criterion 4 (positive): a correct <key>.<leaf> binding resolves — no
// UNKNOWN_BLUEPRINT_KEY when the leading segment names a declared key.
func TestCompile_DeclaredBlueprintKeyBindingResolves(t *testing.T) {
	layout := &CanvasLayout{
		Version: "v1",
		Root: LayoutNode{
			Kind: "stack",
			ID:   "root",
			Children: []LayoutNode{{
				Kind:     "text",
				ID:       "label",
				Bindings: map[string]string{"value": "score.value"},
			}},
		},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": layout},
		blueprints: map[string]*BlueprintGraph{"bp-score": scoreBlueprint("bp-score", "value")},
		manifest:   pureManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{{Key: "score", ID: "bp-score"}}}, f)
	if err != nil {
		t.Fatalf("correct keyed binding rejected: %v", err)
	}
	if _, ok := graphNodeByPath(g, "score.value"); !ok {
		t.Fatalf("declared key leaf missing; nodes = %+v", g.Nodes)
	}
}

// Per-blueprint purity (criterion 6): an impure compute in ANY blueprint of an
// N-blueprint push → IMPURE_COMPUTE.
func TestCompile_NBlueprints_PerBlueprintPurity(t *testing.T) {
	impure := &BlueprintGraph{
		ID: "bp-bad",
		Nodes: []BlueprintNode{{
			ID:      "out.x",
			Compute: "side.effect@1",
			Config:  map[string]json.RawMessage{"name": json.RawMessage(`"leak"`)},
		}},
	}
	manifest := pureManifest()
	manifest["side.effect@1"] = ComputeManifestEntry{IsPure: false, Version: "1"}

	f := &fakeFetcher{
		layouts: map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{
			"bp-good": scoreBlueprint("bp-good", "value"),
			"bp-bad":  impure,
		},
		manifest: manifest,
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{
			{Key: "good", ID: "bp-good"},
			{Key: "bad", ID: "bp-bad"},
		}}, f)
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrImpureCompute) {
		t.Fatalf("want IMPURE_COMPUTE from the bad blueprint, got %v", err)
	}
}
