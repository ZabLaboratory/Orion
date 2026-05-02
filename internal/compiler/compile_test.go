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

// pureManifest is a small manifest with two well-known pure computes.
func pureManifest() ComputeManifest {
	return ComputeManifest{
		"core.add":     {IsPure: true, IsBounded: true, Version: "1"},
		"core.literal": {IsPure: true, IsBounded: true, Version: "1"},
		"core.input":   {IsPure: true, IsBounded: true, Version: "1"},
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
			{ID: "in.a", Compute: "core.input"},
			{ID: "in.b", Compute: "core.input"},
			{ID: "out.sum", Compute: "core.add", OutputAt: "score.team_a"},
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
			{ID: "out.sus", Compute: "side.effect", OutputAt: "score.team_a"},
		},
	}
	manifest := pureManifest()
	manifest["side.effect"] = ComputeManifestEntry{IsPure: false, Version: "1"}

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
			{ID: "out.x", Compute: "nope.never_seen", OutputAt: "score.team_a"},
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
			{ID: "in.a", Compute: "core.input"},
			{ID: "in.b", Compute: "core.input"},
			{ID: "out.sum", Compute: "core.add", OutputAt: "score.team_a"},
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

// User-component operator_inputs are hoisted with instance-path prefix.
func TestCompile_HoistsOperatorInputs(t *testing.T) {
	comp := &UserComponent{
		ID: "team-row",
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
