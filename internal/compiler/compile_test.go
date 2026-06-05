package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeFetcher is the in-memory test double the compiler tests use.
type fakeFetcher struct {
	layouts    map[string]*CanvasLayout
	blueprints map[string]*BlueprintGraph
	components map[ComponentRef]*UserComponent
	manifest   ComputeManifest
	failKind   string
}

func (f *fakeFetcher) FetchCanvasLayout(_ context.Context, v string) (*CanvasLayout, error) {
	if f.failKind == "layout" {
		return nil, errors.New("layout offline")
	}
	if l, ok := f.layouts[v]; ok {
		return l, nil
	}
	return nil, errors.New("layout not found")
}
func (f *fakeFetcher) FetchBlueprint(_ context.Context, id string) (*BlueprintGraph, error) {
	if f.failKind == "blueprint" {
		return nil, errors.New("blueprint offline")
	}
	if b, ok := f.blueprints[id]; ok {
		return b, nil
	}
	return nil, errors.New("blueprint not found")
}
func (f *fakeFetcher) FetchComponent(_ context.Context, ref ComponentRef) (*UserComponent, error) {
	if c, ok := f.components[ref]; ok {
		return c, nil
	}
	return nil, errors.New("component not found")
}
func (f *fakeFetcher) FetchComputeManifest(_ context.Context) (ComputeManifest, error) {
	if f.failKind == "manifest" {
		return nil, errors.New("manifest offline")
	}
	return f.manifest, nil
}

// pureManifest is a small manifest with the well-known pure computes the
// compiler tests reference. Keys are the qualified `namespace.name@version`
// reference (the same string a node carries in `definition`,
// ADR 004 §7.1) — including the sink stdlib nodes core.output@1 /
// core.input@1 / core.literal@1 whose body the §7.2 derivation reads.
func pureManifest() ComputeManifest {
	return ComputeManifest{
		"core.math.add@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.literal@1":  {IsPure: true, IsBounded: true, Version: "1"},
		"core.input@1":    {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":   {IsPure: true, IsBounded: true, Version: "1"},
	}
}

// outputNode builds a core.output@1 sink whose config.name is the leaf
// path the runtime writes to (ADR 004 §7.2). It replaces the old
// `OutputAt:` struct-literal form the body-contract fix removed.
func outputNode(id, leaf string) BlueprintNode {
	return BlueprintNode{
		ID:      id,
		Compute: "core.output@1",
		Config:  map[string]json.RawMessage{"name": json.RawMessage(`"` + leaf + `"`)},
	}
}

// minimalLayout returns a one-node layout (a stack with no children).
func minimalLayout(version string) *CanvasLayout {
	return &CanvasLayout{
		Version: version,
		Root: LayoutNode{
			Kind:  "stack",
			ID:    "root",
			Props: map[string]json.RawMessage{"direction": json.RawMessage(`"vertical"`)},
		},
	}
}

func TestCompile_HappyPath(t *testing.T) {
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
		components: map[ComponentRef]*UserComponent{},
		manifest:   pureManifest(),
	}
	g, b, version, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	if !strings.HasPrefix(version, "sha256:") {
		t.Fatalf("scene_version = %q, want sha256: prefix", version)
	}
	if g.SceneVersion != version || b.SceneVersion != version {
		t.Fatal("scene_version not propagated to artefacts")
	}
	if len(g.Nodes) != 3 {
		t.Fatalf("expected 3 graph nodes, got %d", len(g.Nodes))
	}
	// Topological order: inputs precede the computed.
	if g.Nodes[len(g.Nodes)-1].ID != "out.sum" {
		t.Fatalf("topo order wrong, last node = %q", g.Nodes[len(g.Nodes)-1].ID)
	}
}

// Criterion 17: cycle in user-component graph → CYCLIC_COMPONENT.
func TestCompile_CyclicComponent(t *testing.T) {
	// Component A's body uses B; B's body uses A.
	compA := &UserComponent{
		ID: "comp-a",
		Body: LayoutNode{
			Kind: "comp-b", // references B
			ID:   "child-a",
		},
	}
	compB := &UserComponent{
		ID: "comp-b",
		Body: LayoutNode{
			Kind: "comp-a", // references A
			ID:   "child-b",
		},
	}
	layout := &CanvasLayout{
		Version: "v1",
		Root: LayoutNode{
			Kind:     "stack",
			ID:       "root",
			Children: []LayoutNode{{Kind: "comp-a", ID: "instance-1"}},
		},
	}
	bp := &BlueprintGraph{ID: "bp-1"}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": layout},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		components: map[ComponentRef]*UserComponent{
			{ID: "comp-a", Version: "1"}: compA,
			{ID: "comp-b", Version: "1"}: compB,
		},
		manifest: pureManifest(),
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{
			CanvasVersion:   "v1",
			BlueBlueprintID: "bp-1",
			Components: []ComponentRef{
				{ID: "comp-a", Version: "1"},
				{ID: "comp-b", Version: "1"},
			},
		}, f)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrCyclicComponent) {
		t.Fatalf("expected CYCLIC_COMPONENT, got %v", err)
	}
}

// Criterion 18: blueprint compute flagged is_pure: false → IMPURE_COMPUTE.
func TestCompile_ImpureCompute(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{
				ID:      "out.sus",
				Compute: "side.effect@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"score.team_a"`)},
			},
		},
	}
	manifest := pureManifest()
	manifest["side.effect@1"] = ComputeManifestEntry{IsPure: false, Version: "1"}

	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		manifest:   manifest,
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err == nil {
		t.Fatal("expected purity error")
	}
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrImpureCompute) {
		t.Fatalf("want IMPURE_COMPUTE, got %v", err)
	}
}

// Unknown compute id → UNKNOWN_COMPUTE_NODE.
func TestCompile_UnknownComputeNode(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{ID: "out.x", Compute: "nope.never_seen@1"},
		},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		manifest:   pureManifest(),
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrUnknownComputeNode) {
		t.Fatalf("want UNKNOWN_COMPUTE_NODE, got %v", err)
	}
}

// Identical pushes deterministically hash to the same scene_version.
func TestCompile_DeterministicHash(t *testing.T) {
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
	envelope := PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}
	_, _, v1, err := Compile(context.Background(), "scene-1", envelope, f)
	if err != nil {
		t.Fatal(err)
	}
	_, _, v2, err := Compile(context.Background(), "scene-1", envelope, f)
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Fatalf("non-deterministic hash: %s vs %s", v1, v2)
	}
}

// A blueprint-free scene (layout-only default) compiles cleanly. The
// fetcher is rigged with failKind:"blueprint" so any call to
// FetchBlueprint errors — a green compile therefore PROVES the fetch
// was skipped (issue #28, ADR 007 §8). Both the target sentinel ""
// and the legacy "none" must be tolerated.
func TestCompile_NoBlueprint(t *testing.T) {
	newFetcher := func() *fakeFetcher {
		return &fakeFetcher{
			layouts: map[string]*CanvasLayout{"v1": minimalLayout("v1")},
			// No blueprints registered, and failKind forces
			// FetchBlueprint to error if it is ever called.
			blueprints: map[string]*BlueprintGraph{},
			manifest:   pureManifest(),
			failKind:   "blueprint",
		}
	}

	cases := []struct {
		name string
		bpID string
	}{
		{name: "empty target sentinel", bpID: ""},
		{name: "legacy none sentinel", bpID: "none"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFetcher()
			envelope := PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: tc.bpID}

			g, b, version, err := Compile(context.Background(), "scene-1", envelope, f)
			if err != nil {
				// If the fetch were NOT skipped, failKind:"blueprint"
				// would surface here as ErrFetchUpstream.
				t.Fatalf("blueprint-free compile errored (fetch not skipped?): %v", err)
			}
			if g == nil || b == nil {
				t.Fatal("expected non-nil graph and bundle")
			}
			if !strings.HasPrefix(version, "sha256:") {
				t.Fatalf("scene_version = %q, want sha256: prefix", version)
			}
			if g.SceneVersion != version || b.SceneVersion != version {
				t.Fatal("scene_version not propagated to artefacts")
			}
			// No blueprint means no compute nodes.
			if len(g.Nodes) != 0 {
				t.Fatalf("expected 0 graph nodes for blueprint-free scene, got %d", len(g.Nodes))
			}

			// Determinism: a second identical push hashes byte-equal.
			_, _, version2, err := Compile(context.Background(), "scene-1", envelope, newFetcher())
			if err != nil {
				t.Fatalf("second compile errored: %v", err)
			}
			if version != version2 {
				t.Fatalf("non-deterministic scene_version: %s vs %s", version, version2)
			}
		})
	}
}

// User-component operator_inputs are hoisted with instance-path prefix.
func TestCompile_HoistsOperatorInputs(t *testing.T) {
	comp := &UserComponent{
		ID:   "team-row",
		Body: LayoutNode{Kind: "stack", ID: "body"},
		Inputs: []OperatorInput{
			{Path: "score", Label: "Score", Type: "number"},
		},
	}
	layout := &CanvasLayout{
		Version: "v1",
		Root: LayoutNode{
			Kind: "stack",
			ID:   "root",
			Children: []LayoutNode{
				{Kind: "team-row", ID: "instance-A"},
				{Kind: "team-row", ID: "instance-B"},
			},
		},
	}
	bp := &BlueprintGraph{ID: "bp-1"}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": layout},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		components: map[ComponentRef]*UserComponent{{ID: "team-row", Version: "1"}: comp},
		manifest:   pureManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{
			CanvasVersion:   "v1",
			BlueBlueprintID: "bp-1",
			Components:      []ComponentRef{{ID: "team-row", Version: "1"}},
		}, f)
	if err != nil {
		t.Fatal(err)
	}
	paths := make(map[string]bool)
	for _, in := range g.OperatorInputs {
		paths[in.Path] = true
	}
	// expandLayout walks `instance` IDs into the prefix.
	wantA := "root.instance-A.score"
	wantB := "root.instance-B.score"
	if !paths[wantA] || !paths[wantB] {
		t.Fatalf("missing hoisted paths in %v", paths)
	}
}

// TestBlueprintNode_BodyContract is the contract test for issue #35
// (ADR 004 §7.2). It deserialises a REAL Blue node body — the
// config/inputs/outputs shape Prism emits and Blue stores — and asserts
// that (a) the wire body round-trips into BlueprintNode (Config["name"]
// is populated, not nil) and (b) validateBlueprint derives the right
// runtime GraphNode (Kind="output", Path=config.name).
//
// This test FAILS on the pre-#35 struct: the phantom `output_at`/`args`
// tags meant a real node deserialised with Config==nil and OutputAt=="",
// so Path was "" and Kind was "input" (no upstream) — a silently wrong
// graph. The assertions below pin the post-fix behaviour exactly where
// the old code went wrong (see the inline notes).
func TestBlueprintNode_BodyContract(t *testing.T) {
	// A real core.output@1 node as Prism authors it / Blue stores it
	// (snake_case wire, config.name carries the leaf, typed input port).
	const wire = `{
		"id": "out.sum",
		"definition": "core.output@1",
		"config": {"name": "score.team_a"},
		"inputs": [
			{"id": "p1", "name": "value", "type": "core.primitive.json", "kind": "data", "required": true}
		],
		"outputs": []
	}`

	var node BlueprintNode
	if err := json.Unmarshal([]byte(wire), &node); err != nil {
		t.Fatalf("unmarshal real Blue node body: %v", err)
	}

	// (a) Body round-trips: definition + config are populated. On the
	// pre-#35 struct, Config did not exist and the value was dropped.
	if node.Compute != "core.output@1" {
		t.Fatalf("Compute = %q, want core.output@1 (definition tag, §7.1)", node.Compute)
	}
	rawName, ok := node.Config["name"]
	if !ok {
		t.Fatal("Config[\"name\"] absent — node body did not deserialise (phantom output_at/args regression)")
	}
	var name string
	if err := json.Unmarshal(rawName, &name); err != nil || name != "score.team_a" {
		t.Fatalf("Config[\"name\"] = %q (err %v), want score.team_a", name, err)
	}

	// (b) validateBlueprint derives the runtime GraphNode off the real
	// body. Pre-#35 this produced Kind="input", Path="" (silently wrong).
	bp := &BlueprintGraph{ID: "bp-1", Nodes: []BlueprintNode{node}}
	manifest := ComputeManifest{
		"core.output@1": {IsPure: true, IsBounded: true, Version: "1"},
	}
	nodes, _, diags := validateBlueprint(bp, manifest)
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("unexpected diagnostic: %s %s", d.Code, d.Message)
		}
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 graph node, got %d", len(nodes))
	}
	got := nodes[0]
	if got.Path != "score.team_a" {
		t.Fatalf("GraphNode.Path = %q, want score.team_a (derived from config.name, §7.2)", got.Path)
	}
	if got.Kind != "output" {
		t.Fatalf("GraphNode.Kind = %q, want output (core.output@1 is a sink, §7.2)", got.Kind)
	}
}

// TestBlueprintNode_LiteralSeedsDefault proves the core.literal@1 path of
// the §7.2 derivation: config.value seeds graph.Defaults at the literal's
// own output leaf (replacing the old Args["default"] intent).
func TestBlueprintNode_LiteralSeedsDefault(t *testing.T) {
	const wire = `{
		"id": "lit.threshold",
		"definition": "core.literal@1",
		"config": {"value": 42},
		"outputs": [
			{"id": "p1", "name": "value", "type": "core.primitive.json", "kind": "data"}
		]
	}`
	var node BlueprintNode
	if err := json.Unmarshal([]byte(wire), &node); err != nil {
		t.Fatalf("unmarshal literal node: %v", err)
	}

	bp := &BlueprintGraph{ID: "bp-1", Nodes: []BlueprintNode{node}}
	manifest := ComputeManifest{
		"core.literal@1": {IsPure: true, IsBounded: true, Version: "1"},
	}
	nodes, defaults, diags := validateBlueprint(bp, manifest)
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("unexpected diagnostic: %s %s", d.Code, d.Message)
		}
	}
	if len(nodes) != 1 || nodes[0].Kind != "input" {
		t.Fatalf("literal node: got %d nodes, kind %q; want 1 node kind=input", len(nodes), kindOf(nodes))
	}
	raw, ok := defaults["lit.threshold"]
	if !ok {
		t.Fatal("graph.Defaults missing the literal's output leaf (config.value not seeded)")
	}
	if string(raw) != "42" {
		t.Fatalf("Defaults[\"lit.threshold\"] = %s, want 42", raw)
	}
}

func kindOf(nodes []GraphNode) string {
	if len(nodes) == 0 {
		return "<none>"
	}
	return nodes[0].Kind
}
