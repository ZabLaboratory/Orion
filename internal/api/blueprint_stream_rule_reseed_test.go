package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// bpRuleFetcher serves a blueprint whose exec layer actually compiles (an
// on-start spine → variable.set) AND carries a DATA node (`appid`) feeding the
// exec node's data input by a normal edge — the ADR 017 shape the blueprint-
// direct compile must materialise (before the fix the promoted graph had no
// Nodes, so this data edge resolved to nothing at runtime). promoteBlueprint-
// StreamRule and the boot reseed both go through CompileBlueprintRule, so a
// green promote/reload here proves the full data tranche compiles end-to-end
// through the endpoint. FetchBlueprint echoes the requested id.
type bpRuleFetcher struct{}

func (bpRuleFetcher) FetchCanvasLayout(_ context.Context, v string) (*compiler.CanvasLayout, error) {
	return &compiler.CanvasLayout{Version: v, Root: compiler.LayoutNode{Kind: "stack", ID: "root"}}, nil
}

func (bpRuleFetcher) FetchBlueprint(_ context.Context, id string) (*compiler.BlueprintGraph, error) {
	return &compiler.BlueprintGraph{
		ID: id,
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{{Name: "then", Type: "exec", Kind: "exec"}}},
			{ID: "appid", Compute: "core.input@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"app_id"`)},
				Outputs: []compiler.BlueprintPort{{Name: "out", Type: "any", Kind: "data"}}},
			{ID: "set", Compute: "core.variable.set@1",
				Config: map[string]json.RawMessage{"variable": json.RawMessage(`"x"`)},
				Inputs: []compiler.BlueprintPort{
					{Name: "exec_in", Type: "exec", Kind: "exec"},
					{Name: "value", Type: "any", Kind: "data"}},
				Outputs: []compiler.BlueprintPort{{Name: "then", Type: "exec", Kind: "exec"}}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "appid", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}, nil
}

func (bpRuleFetcher) FetchBlueprintGraph(_ context.Context, _ string, _ int) (*compiler.ResolvedBlueprintGraph, error) {
	return nil, compiler.ErrRefUnresolved
}

func (bpRuleFetcher) FetchComponent(_ context.Context, _ compiler.ComponentRef) (*compiler.UserComponent, error) {
	return nil, compiler.ErrRefUnresolved
}

func (bpRuleFetcher) FetchComputeManifest(_ context.Context) (compiler.ComputeManifest, error) {
	return compiler.ComputeManifest{
		"core.event.on-start@1": {Version: "1"},
		"core.variable.set@1":   {Version: "1"},
		"core.input@1":          {IsPure: true, Version: "1"},
	}, nil
}

// Blueprint-direct stream rule durability across a restart (ADR 009
// Amendment 1, issue #287). The acceptance criterion: a blueprint-direct rule
// promoted before a restart is re-promoted on boot by re-fetching + recompiling
// from Blue — while ONLY its identity is persisted, never any live leaf state.

// TestBlueprintStreamRule_SurvivesRestart proves the boot reseed
// (ReloadBlueprintStreamRules) still works over a persisted rule id,
// seeded directly through the store — the promote/demote HTTP endpoint
// (POST/DELETE /show/stream-rules) is retired (#15, #331; porteur: no
// bluehost multi-instance model is coming) but ReloadBlueprintStreamRules
// itself is kept for continuity over any rule id persisted before the
// retirement, so this still needs coverage.
func TestBlueprintStreamRule_SurvivesRestart(t *testing.T) {
	fetcher := bpRuleFetcher{}
	_, show1, st := switchTestServer(t, fetcher)
	ctx := context.Background()

	bpID := uuid.NewString()
	bpUUID, err := uuid.Parse(bpID)
	if err != nil {
		t.Fatalf("parse bpID: %v", err)
	}

	// Seed exactly what the (now-retired) promote endpoint used to do:
	// promote in-memory + persist the id.
	bp, err := fetcher.FetchBlueprint(ctx, bpID)
	if err != nil {
		t.Fatalf("FetchBlueprint: %v", err)
	}
	graph, cerr := compiler.CompileBlueprintRule(ctx, bp, bpID, fetcher)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	progs, err := runtime.ExecProgramsFromGraph(graph)
	if err != nil {
		t.Fatalf("exec programs: %v", err)
	}
	if err := show1.PromoteBlueprintStreamRule(bpID, graph, &compiler.RenderBundle{}, progs...); err != nil {
		t.Fatalf("PromoteBlueprintStreamRule: %v", err)
	}
	if err := st.AddBlueprintStreamRule(ctx, bpUUID); err != nil {
		t.Fatalf("AddBlueprintStreamRule: %v", err)
	}

	// In-memory: promoted as a BLUEPRINT-kind rule.
	if !show1.IsStreamRule(bpID) {
		t.Fatal("rule not promoted in-memory")
	}
	if kind, ok := show1.StreamRuleKind(bpID); !ok || kind != runtime.RuleKindBlueprint {
		t.Fatalf("kind = %v ok=%v, want RuleKindBlueprint", kind, ok)
	}

	// Persisted: the id (and only the id) is in the durable set.
	ids, err := st.ListBlueprintStreamRules(ctx)
	if err != nil {
		t.Fatalf("ListBlueprintStreamRules: %v", err)
	}
	if len(ids) != 1 || ids[0].String() != bpID {
		t.Fatalf("persisted set = %+v, want [%s]", ids, bpID)
	}

	// ── Simulate a restart: a brand-new Show with NOTHING in memory ──────────
	logger := obs.NewLogger(config.Config{LogLevel: "error", LogFormat: config.LogFormatText})
	show2 := runtime.NewShow(runtime.NewComputeRegistry(), logger)
	t.Cleanup(show2.Stop)
	if show2.IsStreamRule(bpID) {
		t.Fatal("fresh Show should have no rules before reseed")
	}

	// The boot reseed re-fetches + recompiles from Blue and re-promotes.
	ReloadBlueprintStreamRules(ctx, st, fetcher, show2, logger)

	if !show2.IsStreamRule(bpID) {
		t.Fatal("blueprint-direct rule NOT reloaded after restart reseed")
	}
	if kind, ok := show2.StreamRuleKind(bpID); !ok || kind != runtime.RuleKindBlueprint {
		t.Fatalf("reloaded kind = %v ok=%v, want RuleKindBlueprint", kind, ok)
	}
}

// TestBlueprintStreamRule_DemoteDropsPersistence proves demotion removes the
// durable row, so a demoted rule does NOT come back after a restart. Seeded
// directly through the store/show primitives — the demote HTTP endpoint
// (DELETE /show/stream-rules/{id}) is retired (#15, #331), but the
// underlying store methods and DemoteStreamRule remain valid, and
// ReloadBlueprintStreamRules must still respect an empty persisted set.
func TestBlueprintStreamRule_DemoteDropsPersistence(t *testing.T) {
	fetcher := bpRuleFetcher{}
	_, show1, st := switchTestServer(t, fetcher)
	ctx := context.Background()
	bpID := uuid.NewString()
	bpUUID, err := uuid.Parse(bpID)
	if err != nil {
		t.Fatalf("parse bpID: %v", err)
	}

	bp, err := fetcher.FetchBlueprint(ctx, bpID)
	if err != nil {
		t.Fatalf("FetchBlueprint: %v", err)
	}
	graph, cerr := compiler.CompileBlueprintRule(ctx, bp, bpID, fetcher)
	if cerr != nil {
		t.Fatalf("compile: %v", cerr)
	}
	progs, err := runtime.ExecProgramsFromGraph(graph)
	if err != nil {
		t.Fatalf("exec programs: %v", err)
	}
	if err := show1.PromoteBlueprintStreamRule(bpID, graph, &compiler.RenderBundle{}, progs...); err != nil {
		t.Fatalf("PromoteBlueprintStreamRule: %v", err)
	}
	if err := st.AddBlueprintStreamRule(ctx, bpUUID); err != nil {
		t.Fatalf("AddBlueprintStreamRule: %v", err)
	}

	// Demote it, same as the retired DELETE handler did: in-memory demote
	// then drop the persisted row.
	show1.DemoteStreamRule(bpID)
	if err := st.RemoveBlueprintStreamRule(ctx, bpUUID); err != nil {
		t.Fatalf("RemoveBlueprintStreamRule: %v", err)
	}

	// The durable set is now empty → a restart brings nothing back.
	ids, err := st.ListBlueprintStreamRules(ctx)
	if err != nil {
		t.Fatalf("ListBlueprintStreamRules: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("persisted set after demote = %+v, want empty", ids)
	}

	logger := obs.NewLogger(config.Config{LogLevel: "error", LogFormat: config.LogFormatText})
	show2 := runtime.NewShow(runtime.NewComputeRegistry(), logger)
	t.Cleanup(show2.Stop)
	ReloadBlueprintStreamRules(ctx, st, fetcher, show2, logger)
	if show2.IsStreamRule(bpID) {
		t.Fatal("demoted rule must NOT reload after restart")
	}
}
