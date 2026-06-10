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

// Criterion 18 (SUPERSEDED by ADR 006 §3.2): purity is no longer a
// rejection. An impure DATA compute (is_pure:false, NO exec pin) is
// SERVED, not refused — it compiles into a normal data GraphNode.
// Capability is total; proof of termination is the validation gate's
// job, not the compiler's.
func TestCompile_ImpureDataCompute_Accepted(t *testing.T) {
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
	// Impure, but NO exec pin → stays in the data layer.
	manifest["side.effect@1"] = ComputeManifestEntry{IsPure: false, Version: "1"}

	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		manifest:   manifest,
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatalf("impure data compute rejected (ADR 006 §3.2 retired the reject): %v", err)
	}
	if _, ok := graphNodeByID(g, "out.sus"); !ok {
		t.Fatalf("impure data node missing from data graph; nodes = %+v", g.Nodes)
	}
	if len(g.ExecPrograms) != 0 {
		t.Fatalf("data-only scene emitted exec programs: %d", len(g.ExecPrograms))
	}
}

// An impure compute carrying an EXEC pin is routed to the ExecProgram,
// removed from the data graph — the partition, not a rejection
// (ADR 006 §3.1).
func TestCompile_ExecPinNode_RoutedToProgram(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{
				ID:      "set",
				Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"counter"`)},
				Inputs: []BlueprintPort{
					{Name: "exec_in", Type: "exec", Kind: "exec"},
					{Name: "value", Type: "integer", Kind: "data"},
				},
				Outputs: []BlueprintPort{{Name: "then", Type: "exec", Kind: "exec"}},
			},
		},
	}
	manifest := pureManifest()
	manifest["core.variable.set@1"] = ComputeManifestEntry{IsPure: true, Version: "1"}

	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		manifest:   manifest,
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatalf("exec-pin scene rejected: %v", err)
	}
	if _, ok := graphNodeByID(g, "set"); ok {
		t.Fatalf("exec node leaked into the data graph; nodes = %+v", g.Nodes)
	}
	if len(g.ExecPrograms) != 1 {
		t.Fatalf("want 1 exec program, got %d", len(g.ExecPrograms))
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

// TestCompile_SeedsOperatorInputDefault is the M9/D2 contract test: a
// scene declaring a top-level operator_input that carries a `default`
// must (a) seed graph.Defaults[path]=default at compile (so a restart /
// cold boot reseeds the declared value, criterion 11 for operator
// inputs) AND (b) register the path in graph.OperatorInputs (so
// sceneAcceptsPath authorises the operator's write — the push of B is
// not rejected WRITE_FORBIDDEN). Before the fix Defaults was fed only by
// blueprint literals / unwired ports, so the operator-input leaf had no
// boot value and the first capture saw it absent.
func TestCompile_SeedsOperatorInputDefault(t *testing.T) {
	layout := &CanvasLayout{
		Version: "v1",
		Root:    LayoutNode{Kind: "stack", ID: "root"},
		Inputs: []OperatorInput{
			{
				Path:    "headline.text",
				Label:   "Headline",
				Type:    "text",
				Default: json.RawMessage(`"GO LIVE"`),
			},
		},
	}
	bp := &BlueprintGraph{ID: "bp-1"}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": layout},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		components: map[ComponentRef]*UserComponent{},
		manifest:   pureManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatal(err)
	}

	// (a) The declared default is seeded into graph.Defaults at its leaf.
	raw, ok := g.Defaults["headline.text"]
	if !ok {
		t.Fatal("graph.Defaults missing the operator_input leaf — default not seeded (D2 regression)")
	}
	if string(raw) != `"GO LIVE"` {
		t.Fatalf(`Defaults["headline.text"] = %s, want "GO LIVE"`, raw)
	}

	// (b) The path is registered as an accepted input (the set
	// sceneAcceptsPath consults: graph.OperatorInputs). Without this the
	// operator's write to the leaf would be dropped (delivered=false).
	found := false
	for _, in := range g.OperatorInputs {
		if in.Path == "headline.text" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("graph.OperatorInputs missing the operator_input path — write would be rejected")
	}
}

// TestCompile_OperatorInputNoDefaultUnseeded proves an operator_input
// WITHOUT a default leaves its leaf unseeded (no value at cold start —
// unchanged behaviour) while still being registered as acceptable.
func TestCompile_OperatorInputNoDefaultUnseeded(t *testing.T) {
	layout := &CanvasLayout{
		Version: "v1",
		Root:    LayoutNode{Kind: "stack", ID: "root"},
		Inputs: []OperatorInput{
			{Path: "ticker.text", Label: "Ticker", Type: "text"},
		},
	}
	bp := &BlueprintGraph{ID: "bp-1"}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": layout},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		components: map[ComponentRef]*UserComponent{},
		manifest:   pureManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Defaults["ticker.text"]; ok {
		t.Fatal("operator_input with no default must not seed Defaults")
	}
	found := false
	for _, in := range g.OperatorInputs {
		if in.Path == "ticker.text" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("operator_input must still be registered as acceptable even without a default")
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
	nodes, _, diags := validateBlueprint(bp, manifest, nil)
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
	nodes, defaults, diags := validateBlueprint(bp, manifest, nil)
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

// TestCompile_DBNodeOutsideQuery (ADR 006 §3.5 / issue #107): a
// core.db.* inline-only atom placed in the main blueprint graph must
// produce DB_NODE_OUTSIDE_QUERY — not UNKNOWN_COMPUTE_NODE and not a
// silent pass. Tests all 6 atoms and verifies the error code.
func TestCompile_DBNodeOutsideQuery(t *testing.T) {
	inlineOnlyAtoms := []string{
		"core.db.from@1",
		"core.db.join@1",
		"core.db.limit@1",
		"core.db.order@1",
		"core.db.select@1",
		"core.db.where@1",
	}
	for _, atom := range inlineOnlyAtoms {
		t.Run(atom, func(t *testing.T) {
			bp := &BlueprintGraph{
				ID: "bp-inline-only",
				Nodes: []BlueprintNode{
					{
						ID:      "bad-node",
						Compute: atom,
					},
				},
			}
			f := &fakeFetcher{
				layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
				blueprints: map[string]*BlueprintGraph{"bp-inline-only": bp},
				manifest:   pureManifest(),
			}
			_, _, _, err := Compile(context.Background(), "scene-1",
				PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-inline-only"}, f)
			if err == nil {
				t.Fatalf("%s in main graph: expected DB_NODE_OUTSIDE_QUERY error, got nil", atom)
			}
			var ce *CompileError
			if !errors.As(err, &ce) {
				t.Fatalf("%s in main graph: expected *CompileError, got %T: %v", atom, err, err)
			}
			if !ce.HasCode(ErrDBNodeOutsideQuery) {
				t.Fatalf("%s in main graph: want DB_NODE_OUTSIDE_QUERY, got diagnostics: %v",
					atom, ce.Diagnostics.Items)
			}
			// Verify it is NOT classified as UNKNOWN_COMPUTE_NODE — the
			// diagnostic must be structural, not a capability rejection.
			if ce.HasCode(ErrUnknownComputeNode) {
				t.Errorf("%s in main graph: must not produce UNKNOWN_COMPUTE_NODE "+
					"(the atom is inline-only, not unknown)", atom)
			}
		})
	}
}

// TestCompile_DBQueryIsServedNormally (ADR 006 §3.5 regression guard):
// core.db.query@1 itself must NOT be treated as inline-only and must
// compile as a normal exec-op node (no DB_NODE_OUTSIDE_QUERY diagnostic).
func TestCompile_DBQueryIsServedNormally(t *testing.T) {
	manifest := pureManifest()
	// core.db.query@1 is impure (is_pure: false in the real manifest) —
	// add it to the test manifest as impure so validateBlueprint doesn't
	// reject it for purity. The inline-only guard must NOT intercept it.
	// We use IsPure:false to match the real manifest; the inline-only
	// guard must fire BEFORE the purity check, and query@1 is not
	// inline-only, so the purity check is what rejects it — not
	// DB_NODE_OUTSIDE_QUERY.
	manifest["core.db.query@1"] = ComputeManifestEntry{IsPure: false, Version: "1"}

	bp := &BlueprintGraph{
		ID: "bp-query",
		Nodes: []BlueprintNode{
			{ID: "q", Compute: "core.db.query@1"},
		},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-query": bp},
		manifest:   manifest,
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-query"}, f)
	if err == nil {
		// If purity rejects it that's fine; what matters is it's not
		// DB_NODE_OUTSIDE_QUERY.
		return
	}
	var ce *CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("unexpected error type: %T: %v", err, err)
	}
	if ce.HasCode(ErrDBNodeOutsideQuery) {
		t.Fatalf("core.db.query@1 must NOT produce DB_NODE_OUTSIDE_QUERY — " +
			"it is a standalone exec-op, not inline-only")
	}
}
