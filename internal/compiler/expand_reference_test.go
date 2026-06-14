package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// These tests exercise the ADR 014 blueprint-reference compile-time
// expansion pass: a scene blueprint carrying a `reference:
// {blueprint_id, version}` node is flattened, BEFORE manifest validation, into
// the referenced sub-graph's core.* nodes, recursively. The exhaustive
// conformance / perf / determinism matrix is Probe (#180); these are Forge's
// proximity tests — one per resolution-criterion clause (ADR 014 §6).

// refManifest is pureManifest plus the computes the referenced sub-graphs use.
func refManifest() ComputeManifest {
	m := pureManifest()
	m["core.math.add@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	m["core.math.multiply@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	return m
}

// inputNode builds a core.input@1 whose config.name is its interface name —
// the join key the expansion pass matches a reference node's ports against.
func inputNode(id, name string) BlueprintNode {
	return BlueprintNode{
		ID:      id,
		Compute: coreInput,
		Config:  map[string]json.RawMessage{"name": json.RawMessage(`"` + name + `"`)},
	}
}

// refNode builds a blueprint-CALL node: a reference to (blueprintID, version).
func refNode(id, blueprintID string, version int) BlueprintNode {
	return BlueprintNode{
		ID:        id,
		Compute:   "blueprint.reference", // definition is decorative for a ref node
		Reference: &BlueprintReference{BlueprintID: blueprintID, Version: version},
	}
}

// doublerGraph is a referenced function: out = add(in, in). Interface input
// "x", output "result". The interior node is core.math.add@1.
func doublerGraph(id string, version int) *ResolvedBlueprintGraph {
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			inputNode("in", "x"),
			{ID: "add", Compute: "core.math.add@1"},
			outputNode("out", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "in", FromPort: "value", ToNode: "add", ToPort: "a"},
			{FromNode: "in", FromPort: "value", ToNode: "add", ToPort: "b"},
			{FromNode: "add", FromPort: "sum", ToNode: "out", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float", Required: true}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float", Required: true}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

func compileWithRefs(t *testing.T, bp *BlueprintGraph, graphs map[string]*ResolvedBlueprintGraph) (*Graph, *fakeFetcher, error) {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-scene": bp},
		components: map[ComponentRef]*UserComponent{},
		manifest:   refManifest(),
		graphs:     graphs,
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, f)
	return g, f, err
}

// RC #2 — a scene blueprint with a `reference` node compiles into a 100%
// core.* flat graph; the call node's ports map onto the function's interface
// pins; the inlined ids are alpha-renamed and collide with nothing.
func TestExpand_SimpleReference_FlatGraph(t *testing.T) {
	// scene: source(in "seed") → ref(double) → output "score.final".
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("dbl", "bp-double", 3),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "dbl", ToPort: "x"},
			{FromNode: "dbl", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	g, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-double@3": doublerGraph("bp-double", 3),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// No `reference` node survives: every graph node is a known flat compute.
	for _, n := range g.Nodes {
		if n.Compute == "blueprint.reference" {
			t.Fatalf("reference node %s leaked into the runtime graph", n.ID)
		}
	}
	// The interior add node is present, alpha-renamed (prefixed), not "add".
	var addID string
	for _, n := range g.Nodes {
		if n.Compute == "core.math.add@1" {
			addID = n.ID
		}
	}
	if addID == "" {
		t.Fatalf("inlined core.math.add@1 missing from expanded graph: %+v", g.Nodes)
	}
	if addID == "add" || !strings.Contains(addID, "__bpref") {
		t.Fatalf("inlined id %q not alpha-renamed (want a __bpref prefix)", addID)
	}
	// The function's interface nodes (core.input@1 "x" / core.output@1
	// "result") are DROPPED — spliced through. The two surviving leaf-bound
	// nodes are the scene's own seed (input) and sink (output).
	var inputs, outputs int
	for _, n := range g.Nodes {
		switch n.Kind {
		case "input":
			inputs++
		case "output":
			outputs++
		}
	}
	if inputs != 1 || outputs != 1 {
		t.Fatalf("expected 1 input + 1 output leaf (scene seed/sink, function pins spliced), got %d/%d: %+v", inputs, outputs, g.Nodes)
	}
	// Edge splice: the add node consumes the scene seed directly (both a/b),
	// and the scene sink consumes the add's sum directly.
	var addUpstreamFromSeed, sinkUpstreamFromAdd bool
	for _, n := range g.Nodes {
		if n.ID == addID {
			for _, u := range n.Upstream {
				if u == "seed" {
					addUpstreamFromSeed = true
				}
			}
		}
		if n.Kind == "output" {
			for _, u := range n.Upstream {
				if u == addID {
					sinkUpstreamFromAdd = true
				}
			}
		}
	}
	if !addUpstreamFromSeed {
		t.Errorf("add node not wired from scene seed (input splice failed)")
	}
	if !sinkUpstreamFromAdd {
		t.Errorf("scene sink not wired from add (output splice failed)")
	}
}

// RC #3 — a function that references ANOTHER function expands on N levels.
func TestExpand_NestedReference_Recurses(t *testing.T) {
	// quad = double(double(x)): bp-quad's graph has a reference to bp-double.
	quad := &ResolvedBlueprintGraph{
		BlueprintID: "bp-quad",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("qin", "x"),
			refNode("inner", "bp-double", 3), // nested reference
			outputNode("qout", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "qin", FromPort: "value", ToNode: "inner", ToPort: "x"},
			{FromNode: "inner", FromPort: "result", ToNode: "qout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("q", "bp-quad", 1),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "q", ToPort: "x"},
			{FromNode: "q", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	g, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-quad@1":   quad,
		"bp-double@3": doublerGraph("bp-double", 3),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// The nested double should be expanded too → its add node is present.
	var adds int
	for _, n := range g.Nodes {
		if n.Compute == "core.math.add@1" {
			adds++
		}
		if n.Compute == "blueprint.reference" {
			t.Fatalf("nested reference %s not expanded", n.ID)
		}
	}
	if adds != 1 {
		t.Fatalf("expected exactly 1 inlined add (from the nested double), got %d", adds)
	}
}

// RC #5 — a version that does not exist / is not published → typed
// BLUEPRINT_REF_UNRESOLVED, push fails closed.
func TestExpand_UnpublishedVersion_Unresolved(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("dbl", "bp-double", 99), // 99 not in graphs → unresolved
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "dbl", ToPort: "x"},
			{FromNode: "dbl", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	_, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-double@3": doublerGraph("bp-double", 3),
	})
	assertHasCode(t, err, ErrBlueprintRefUnresolved)
}

// RC #5 — pinning: two DIFFERENT versions of the same function produce
// different expansions (here v3 doubles, v4 triples) → different hashes.
func TestExpand_PinnedVersions_DistinctExpansions(t *testing.T) {
	tripler := &ResolvedBlueprintGraph{
		BlueprintID: "bp-double",
		Version:     4,
		Nodes: []BlueprintNode{
			inputNode("in", "x"),
			{ID: "mul", Compute: "core.math.multiply@1"},
			outputNode("out", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "in", FromPort: "value", ToNode: "mul", ToPort: "a"},
			{FromNode: "mul", FromPort: "product", ToNode: "out", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	graphs := map[string]*ResolvedBlueprintGraph{
		"bp-double@3": doublerGraph("bp-double", 3),
		"bp-double@4": tripler,
	}
	mkScene := func(v int) *BlueprintGraph {
		return &BlueprintGraph{
			ID: "bp-scene",
			Nodes: []BlueprintNode{
				inputNode("seed", "score.seed"),
				refNode("dbl", "bp-double", v),
				outputNode("sink", "score.final"),
			},
			Edges: []BlueprintEdge{
				{FromNode: "seed", FromPort: "value", ToNode: "dbl", ToPort: "x"},
				{FromNode: "dbl", FromPort: "result", ToNode: "sink", ToPort: "value"},
			},
		}
	}
	f3 := &fakeFetcher{layouts: map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-scene": mkScene(3)},
		components: map[ComponentRef]*UserComponent{}, manifest: refManifest(), graphs: graphs}
	f4 := &fakeFetcher{layouts: map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-scene": mkScene(4)},
		components: map[ComponentRef]*UserComponent{}, manifest: refManifest(), graphs: graphs}

	_, _, v3, err3 := Compile(context.Background(), "s", PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, f3)
	_, _, v4, err4 := Compile(context.Background(), "s", PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, f4)
	if err3 != nil || err4 != nil {
		t.Fatalf("compile errors: v3=%v v4=%v", err3, err4)
	}
	if v3 == v4 {
		t.Fatalf("two different referenced versions produced the same scene_version %q — pinning lost", v3)
	}
}

// RC #7 — determinism: two pushes of the same scene + same referenced version
// produce the byte-identical scene_version hash.
func TestExpand_Deterministic(t *testing.T) {
	bp := func() *BlueprintGraph {
		return &BlueprintGraph{
			ID: "bp-scene",
			Nodes: []BlueprintNode{
				inputNode("seed", "score.seed"),
				refNode("dbl", "bp-double", 3),
				outputNode("sink", "score.final"),
			},
			Edges: []BlueprintEdge{
				{FromNode: "seed", FromPort: "value", ToNode: "dbl", ToPort: "x"},
				{FromNode: "dbl", FromPort: "result", ToNode: "sink", ToPort: "value"},
			},
		}
	}
	g1, _, err1 := compileWithRefs(t, bp(), map[string]*ResolvedBlueprintGraph{"bp-double@3": doublerGraph("bp-double", 3)})
	g2, _, err2 := compileWithRefs(t, bp(), map[string]*ResolvedBlueprintGraph{"bp-double@3": doublerGraph("bp-double", 3)})
	if err1 != nil || err2 != nil {
		t.Fatalf("compile errors: %v %v", err1, err2)
	}
	if g1.SceneVersion != g2.SceneVersion {
		t.Fatalf("non-deterministic expansion: %q != %q", g1.SceneVersion, g2.SceneVersion)
	}
}

// Issue #179 — a self-referential function (A→A) is a cycle: rejected with
// the precise CYCLIC_BLUEPRINT_REFERENCE, not the size bound, and the message
// cites the offending key chain.
func TestExpand_SelfReference_Cyclic(t *testing.T) {
	selfRef := &ResolvedBlueprintGraph{
		BlueprintID: "bp-loop",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("lin", "x"),
			refNode("again", "bp-loop", 1), // references itself
			outputNode("lout", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "lin", FromPort: "value", ToNode: "again", ToPort: "x"},
			{FromNode: "again", FromPort: "result", ToNode: "lout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("l", "bp-loop", 1),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "l", ToPort: "x"},
			{FromNode: "l", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	_, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{"bp-loop@1": selfRef})
	assertHasCode(t, err, ErrCyclicBlueprintReference)
	// The diagnostic must cite the cycle chain (the repeated key).
	assertMessageContains(t, err, ErrCyclicBlueprintReference, "bp-loop@1")
}

// Issue #179 — an INDIRECT cycle A→B→A is rejected with
// CYCLIC_BLUEPRINT_REFERENCE; the chain in the message names both keys.
func TestExpand_IndirectCycle_Cyclic(t *testing.T) {
	// bp-a references bp-b; bp-b references bp-a → loop on the path.
	bpA := &ResolvedBlueprintGraph{
		BlueprintID: "bp-a",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("ain", "x"),
			refNode("toB", "bp-b", 1),
			outputNode("aout", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "ain", FromPort: "value", ToNode: "toB", ToPort: "x"},
			{FromNode: "toB", FromPort: "result", ToNode: "aout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	bpB := &ResolvedBlueprintGraph{
		BlueprintID: "bp-b",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("bin", "x"),
			refNode("toA", "bp-a", 1), // back-edge → cycle
			outputNode("bout", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "bin", FromPort: "value", ToNode: "toA", ToPort: "x"},
			{FromNode: "toA", FromPort: "result", ToNode: "bout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("a", "bp-a", 1),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "a", ToPort: "x"},
			{FromNode: "a", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	_, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-a@1": bpA,
		"bp-b@1": bpB,
	})
	assertHasCode(t, err, ErrCyclicBlueprintReference)
	assertMessageContains(t, err, ErrCyclicBlueprintReference, "bp-a@1")
	assertMessageContains(t, err, ErrCyclicBlueprintReference, "bp-b@1")
}

// Issue #179 — a DAG is NOT a cycle. A diamond A→B, A→C, B→D, C→D reuses D on
// two sibling branches: D is "seen" twice (fetched once, memoised) but never
// twice on a single resolution path, so the whole graph expands WITHOUT error.
func TestExpand_Diamond_NoCycle(t *testing.T) {
	// d = double (interior add). Reused by both b and c.
	d := doublerGraph("bp-d", 1)
	mkMid := func(id string) *ResolvedBlueprintGraph {
		return &ResolvedBlueprintGraph{
			BlueprintID: id,
			Version:     1,
			Nodes: []BlueprintNode{
				inputNode("min", "x"),
				refNode("toD", "bp-d", 1), // both B and C reference the same D
				outputNode("mout", "result"),
			},
			Edges: []BlueprintEdge{
				{FromNode: "min", FromPort: "value", ToNode: "toD", ToPort: "x"},
				{FromNode: "toD", FromPort: "result", ToNode: "mout", ToPort: "value"},
			},
			Interface: BlueprintInterface{
				Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
				Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
			},
			Purity: BlueprintPurity{IsPure: true, IsBounded: true},
		}
	}
	// a = top of the diamond: feeds the same seed into B and C, then sums
	// their two results. References both bp-b and bp-c.
	a := &ResolvedBlueprintGraph{
		BlueprintID: "bp-a",
		Version:     1,
		Nodes: []BlueprintNode{
			inputNode("ain", "x"),
			refNode("toB", "bp-b", 1),
			refNode("toC", "bp-c", 1),
			{ID: "join", Compute: "core.math.add@1"},
			outputNode("aout", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "ain", FromPort: "value", ToNode: "toB", ToPort: "x"},
			{FromNode: "ain", FromPort: "value", ToNode: "toC", ToPort: "x"},
			{FromNode: "toB", FromPort: "result", ToNode: "join", ToPort: "a"},
			{FromNode: "toC", FromPort: "result", ToNode: "join", ToPort: "b"},
			{FromNode: "join", FromPort: "sum", ToNode: "aout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("a", "bp-a", 1),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "a", ToPort: "x"},
			{FromNode: "a", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	g, f, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-a@1": a,
		"bp-b@1": mkMid("bp-b"),
		"bp-c@1": mkMid("bp-c"),
		"bp-d@1": d,
	})
	if err != nil {
		t.Fatalf("diamond DAG must expand without error, got: %v", err)
	}
	// D is reached on two branches (B and C) → expanded twice, two disjoint
	// add nodes from D, plus the one join add in A = 3 adds total. No reference
	// node survives.
	var adds int
	for _, n := range g.Nodes {
		if n.Compute == "blueprint.reference" {
			t.Fatalf("reference node %s leaked: diamond not fully expanded", n.ID)
		}
		if n.Compute == "core.math.add@1" {
			adds++
		}
	}
	if adds != 3 {
		t.Fatalf("expected 3 inlined adds (D on each of 2 branches + A's join), got %d", adds)
	}
	// D fetched ONCE despite two expansion sites (memoised), proving "seen
	// twice" is not "on the path twice".
	if n := f.graphCalls["bp-d@1"]; n != 1 {
		t.Fatalf("FetchBlueprintGraph for bp-d@1 called %d times, want 1 (memoised across DAG branches)", n)
	}
}

// Issue #179 — a deep but ACYCLIC chain within the depth bound expands without
// error (it is neither a cycle nor over the size bound). Chain length 8 < 16.
func TestExpand_DeepAcyclicChain_OK(t *testing.T) {
	const chain = 8
	graphs := map[string]*ResolvedBlueprintGraph{}
	// Link k references link k+1; the last link is a plain doubler.
	for k := 0; k < chain; k++ {
		id := chainID(k)
		if k == chain-1 {
			graphs[id+"@1"] = doublerGraph(id, 1)
			continue
		}
		next := chainID(k + 1)
		graphs[id+"@1"] = &ResolvedBlueprintGraph{
			BlueprintID: id,
			Version:     1,
			Nodes: []BlueprintNode{
				inputNode("cin", "x"),
				refNode("nxt", next, 1),
				outputNode("cout", "result"),
			},
			Edges: []BlueprintEdge{
				{FromNode: "cin", FromPort: "value", ToNode: "nxt", ToPort: "x"},
				{FromNode: "nxt", FromPort: "result", ToNode: "cout", ToPort: "value"},
			},
			Interface: BlueprintInterface{
				Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
				Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
			},
			Purity: BlueprintPurity{IsPure: true, IsBounded: true},
		}
	}
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("head", chainID(0), 1),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "head", ToPort: "x"},
			{FromNode: "head", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	g, _, err := compileWithRefs(t, bp, graphs)
	if err != nil {
		t.Fatalf("deep acyclic chain (len %d < bound) must compile, got: %v", chain, err)
	}
	for _, n := range g.Nodes {
		if n.Compute == "blueprint.reference" {
			t.Fatalf("reference node %s leaked: deep chain not fully expanded", n.ID)
		}
	}
}

func chainID(k int) string { return "bp-chain-" + string(rune('a'+k)) }

// A reference node wiring a port the function does not declare is a
// BLUEPRINT_REF_UNRESOLVED reject (cannot inline an undeclared pin).
func TestExpand_UnknownPort_Rejected(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("dbl", "bp-double", 3),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "dbl", ToPort: "nonesuch"}, // bad pin
			{FromNode: "dbl", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	_, _, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{"bp-double@3": doublerGraph("bp-double", 3)})
	assertHasCode(t, err, ErrBlueprintRefUnresolved)
}

// Memoisation: a function referenced from two sites is fetched ONCE per
// compile (ADR 014 §3 / ADR 012 memoised push-time fetch).
func TestExpand_MemoisesFetchPerVersion(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("a", "bp-double", 3),
			refNode("b", "bp-double", 3), // same pinned pair, second site
			outputNode("sink1", "score.a"),
			outputNode("sink2", "score.b"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "a", ToPort: "x"},
			{FromNode: "seed", FromPort: "value", ToNode: "b", ToPort: "x"},
			{FromNode: "a", FromPort: "result", ToNode: "sink1", ToPort: "value"},
			{FromNode: "b", FromPort: "result", ToNode: "sink2", ToPort: "value"},
		},
	}
	g, f, err := compileWithRefs(t, bp, map[string]*ResolvedBlueprintGraph{"bp-double@3": doublerGraph("bp-double", 3)})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if n := f.graphCalls["bp-double@3"]; n != 1 {
		t.Fatalf("FetchBlueprintGraph called %d times for bp-double@3, want 1 (memoised)", n)
	}
	// Two expansion sites → two disjoint add nodes, no id collision.
	var adds int
	ids := map[string]struct{}{}
	for _, n := range g.Nodes {
		if _, dup := ids[n.ID]; dup {
			t.Fatalf("duplicate node id %q after two expansions of the same function (alpha-rename collision)", n.ID)
		}
		ids[n.ID] = struct{}{}
		if n.Compute == "core.math.add@1" {
			adds++
		}
	}
	if adds != 2 {
		t.Fatalf("expected 2 inlined adds (one per site), got %d", adds)
	}
}

func assertHasCode(t *testing.T, err error, code DiagnosticCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected compile error with code %s, got nil", code)
	}
	var ce *CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *CompileError", err)
	}
	if !ce.HasCode(code) {
		t.Fatalf("compile error missing code %s: %+v", code, ce.Diagnostics.Items)
	}
}

// assertMessageContains checks that the diagnostic with the given code carries
// a message mentioning `want` — used to prove a cycle diagnostic cites the
// offending key chain.
func assertMessageContains(t *testing.T, err error, code DiagnosticCode, want string) {
	t.Helper()
	var ce *CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *CompileError", err)
	}
	for _, it := range ce.Diagnostics.Items {
		if it.Code == code && strings.Contains(it.Message, want) {
			return
		}
	}
	t.Fatalf("no %s diagnostic message contains %q: %+v", code, want, ce.Diagnostics.Items)
}
