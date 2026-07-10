package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// bpRuleFetcher serves a blueprint whose exec layer actually compiles (an
// on-start spine → variable.set), so promoteBlueprintStreamRule and the boot
// reseed both produce a real exec program. FetchBlueprint echoes the requested
// id, so any blueprint_id resolves.
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
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"x"`)},
				Inputs:  []compiler.BlueprintPort{{Name: "exec_in", Type: "exec", Kind: "exec"}},
				Outputs: []compiler.BlueprintPort{{Name: "then", Type: "exec", Kind: "exec"}}},
		},
		Edges: []compiler.BlueprintEdge{{FromNode: "start", FromPort: "then", ToNode: "set", ToPort: "exec_in"}},
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
	}, nil
}

// Blueprint-direct stream rule durability across a restart (ADR 009
// Amendment 1, issue #287). The acceptance criterion: a blueprint-direct rule
// promoted before a restart is re-promoted on boot by re-fetching + recompiling
// from Blue — while ONLY its identity is persisted, never any live leaf state.

// TestBlueprintStreamRule_SurvivesRestart drives the full lifecycle over the
// pure-Go SQLite store: promote a blueprint-direct rule via the HTTP endpoint
// (which persists its id), then simulate a process restart with a FRESH Show
// and prove ReloadBlueprintStreamRules brings the rule back.
func TestBlueprintStreamRule_SurvivesRestart(t *testing.T) {
	fetcher := bpRuleFetcher{}
	srv, show1, st := switchTestServer(t, fetcher)
	ctx := context.Background()

	bpID := uuid.NewString()

	// Promote the blueprint-direct rule via the real endpoint.
	body, _ := json.Marshal(map[string]string{"blueprint_id": bpID})
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/show/stream-rules", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "op")
	req.Header.Set("X-Authenticated-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("promote request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("promote: got %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// In-memory: promoted as a BLUEPRINT-kind rule.
	if !show1.IsStreamRule(bpID) {
		t.Fatal("rule not promoted in-memory after POST")
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
// durable row, so a demoted rule does NOT come back after a restart.
func TestBlueprintStreamRule_DemoteDropsPersistence(t *testing.T) {
	fetcher := bpRuleFetcher{}
	srv, _, st := switchTestServer(t, fetcher)
	ctx := context.Background()
	bpID := uuid.NewString()

	promote := func() {
		body, _ := json.Marshal(map[string]string{"blueprint_id": bpID})
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/show/stream-rules", bytes.NewReader(body))
		req.Header.Set("X-Authenticated-User", "op")
		req.Header.Set("X-Authenticated-Role", "operator")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("promote: %v", err)
		}
		resp.Body.Close()
	}
	promote()

	// Demote it via DELETE.
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/v1/show/stream-rules/"+bpID, nil)
	req.Header.Set("X-Authenticated-User", "op")
	req.Header.Set("X-Authenticated-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("demote: got %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

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
