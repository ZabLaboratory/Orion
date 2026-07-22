package compiler

import (
	"context"
	"encoding/json"
	"testing"
)

// Unit tests for CompileBlueprintRule (ADR 017): the blueprint-direct compile
// entrypoint that materialises the FULL graph (Nodes/Bindings/Defaults/
// ExecPrograms) for a stream rule, closing the empty-Nodes silent-drop the
// exec-only simulate seam (CompileExecPrograms) leaves.

// bpRuleManifest is the served set the blueprint-direct tests reference: the
// exec ops are validated by conformance (not the manifest), the data computes
// by validateBlueprint's manifest lookup.
func bpRuleManifest() ComputeManifest {
	return ComputeManifest{
		"core.event.on-start@1": {Version: "1"},
		"core.variable.set@1":   {Version: "1"},
		"core.input@1":          {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":         {IsPure: true, IsBounded: true, Version: "1"},
	}
}

// execDataFedBlueprint is an on-start spine whose exec node (`set`) reads a data
// input (`value`) fed by a normal data edge from a data node (`appid`) — the
// ADR 017 marker shape (app_id on core.overlay-app.set@1). Modelled with the
// well-known served computes so the tests need no bespoke registry entries.
func execDataFedBlueprint(id string) *BlueprintGraph {
	return &BlueprintGraph{
		ID: id,
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1", Outputs: []BlueprintPort{execOutP("then")}},
			{ID: "appid", Compute: "core.input@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"app_id"`)},
				Outputs: []BlueprintPort{dataP("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"x"`)},
				Inputs:  []BlueprintPort{execInP("exec_in"), dataP("value")},
				Outputs: []BlueprintPort{execOutP("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "appid", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}
}

// RC-1: a blueprint whose exec input is fed by a data-node edge compiles to a
// graph that CARRIES the data node (so nodeIdx is populated at runtime) AND
// exactly one exec program. Before ADR 017 the promoted graph had zero Nodes.
func TestCompileBlueprintRule_MaterialisesDataTranche(t *testing.T) {
	f := &fakeFetcher{manifest: bpRuleManifest()}
	g, cerr := CompileBlueprintRule(context.Background(), execDataFedBlueprint("bp-1"), "", f)
	if cerr != nil {
		t.Fatalf("unexpected compile error: %v", cerr)
	}
	if len(g.ExecPrograms) != 1 {
		t.Fatalf("want 1 exec program, got %d", len(g.ExecPrograms))
	}
	// The data node `appid` must be present in the materialised graph — the
	// exec nodes (`start`, `set`) are routed to the ExecProgram and excluded.
	var found bool
	for _, n := range g.Nodes {
		if n.ID == "appid" {
			found = true
		}
	}
	if !found {
		t.Fatalf("data node `appid` not materialised into graph; Nodes=%+v", g.Nodes)
	}
	if g.SceneID != "bp-1" {
		t.Fatalf("SceneID = %q, want bp-1", g.SceneID)
	}
}

// RC-2: parity. The SAME blueprint compiled through the scene path (Compile,
// singular envelope → key "") and through CompileBlueprintRule (key "") yields
// byte-identical data Nodes and Defaults — the substance the runtime resolves.
func TestCompileBlueprintRule_ParityWithScenePath(t *testing.T) {
	bp := execDataFedBlueprint("bp-1")
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		components: map[ComponentRef]*UserComponent{},
		manifest:   bpRuleManifest(),
	}
	sceneGraph, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatalf("scene compile error: %v", err)
	}
	ruleGraph, cerr := CompileBlueprintRule(context.Background(), bp, "", f)
	if cerr != nil {
		t.Fatalf("rule compile error: %v", cerr)
	}

	sceneNodes, _ := json.Marshal(sceneGraph.Nodes)
	ruleNodes, _ := json.Marshal(ruleGraph.Nodes)
	if string(sceneNodes) != string(ruleNodes) {
		t.Fatalf("data Nodes diverge:\n scene=%s\n  rule=%s", sceneNodes, ruleNodes)
	}
	sceneDefs, _ := json.Marshal(sceneGraph.Defaults)
	ruleDefs, _ := json.Marshal(ruleGraph.Defaults)
	if string(sceneDefs) != string(ruleDefs) {
		t.Fatalf("Defaults diverge:\n scene=%s\n  rule=%s", sceneDefs, ruleDefs)
	}
}

// RC-7: a pure-dataflow blueprint (no exec spine) is rejected fail-loud
// (NO_EXEC_PROGRAM) — a stream rule needs an entrypoint to run. A valid SCENE,
// not a valid RULE.
func TestCompileBlueprintRule_RejectsPureDataflow(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{ID: "in.a", Compute: "core.input@1"},
			outputNode("out.x", "score.a"),
		},
		Edges: []BlueprintEdge{{FromNode: "in.a", ToNode: "out.x", FromPort: "out", ToPort: "x"}},
	}
	f := &fakeFetcher{manifest: bpRuleManifest()}
	_, cerr := CompileBlueprintRule(context.Background(), bp, "", f)
	if cerr == nil {
		t.Fatal("expected a compile error for a pure-dataflow blueprint")
	}
	if !cerr.HasCode(ErrNoExecProgram) {
		t.Fatalf("want NO_EXEC_PROGRAM, got %v", cerr)
	}
}

// A manifest fetch failure surfaces as a compile error (fail-closed), never a
// partial graph.
func TestCompileBlueprintRule_ManifestFetchFails(t *testing.T) {
	f := &fakeFetcher{manifest: bpRuleManifest(), failKind: "manifest"}
	_, cerr := CompileBlueprintRule(context.Background(), execDataFedBlueprint("bp-1"), "", f)
	if cerr == nil {
		t.Fatal("expected a compile error when the manifest is offline")
	}
}
