package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Cockpit contract surface tests (Orion #210, Blue ADR 008 §3.5) at the HTTP
// boundary: authz, stream_id requirement, facet aggregation (params/triggers/
// awaits), scope (scene vs stream), dormant-blueprint exclusion, and the
// pending-await derivation.

const (
	cockpitActiveID = "22222222-2222-2222-2222-222222222222"
	cockpitRuleID   = "33333333-3333-3333-3333-333333333333"
	cockpitOtherID  = "44444444-4444-4444-4444-444444444444"
)

// cockpitFixture builds a Show with:
//   - an ACTIVE scene (blueprint "bp") with a declared operator input
//     (param), an on-call entrypoint (trigger) and an on-start-armed await
//     (pending) — scope `scene`;
//   - a promoted STREAM-LEVEL rule scene (blueprint "rule") with its own
//     on-call entrypoint — scope `stream`;
//   - a DORMANT roster scene (blueprint "ghost") that is neither active nor a
//     rule — its contracts must NOT appear.
type cockpitFixture struct {
	mux  *http.ServeMux
	show *runtime.Show
}

func awaitNode(id, name, valueType string) *runtime.ExecNode {
	return &runtime.ExecNode{
		ID: id, Op: runtime.OpOperatorAwait,
		Config: map[string]json.RawMessage{
			"await_name": json.RawMessage(`"` + name + `"`),
			"value_type": json.RawMessage(`"` + valueType + `"`),
		},
		Next: map[string]runtime.ExecTarget{"then": {Node: "sink"}},
	}
}

func newCockpitFixture(t *testing.T) *cockpitFixture {
	t.Helper()
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	show.SetExecMetrics(m)
	t.Cleanup(show.Stop)

	// --- active scene: param + on-call trigger + armed await -------------
	activeGraph := &compiler.Graph{SceneID: cockpitActiveID, SceneVersion: "sha256:a"}
	activeBundle := &compiler.RenderBundle{
		SceneVersion: "sha256:a",
		OperatorInputs: []compiler.OperatorInput{
			{Path: "__inputs.bp.title", Label: "Title", Type: "text"},
		},
	}
	activeProg := &runtime.ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*runtime.ExecNode{
			"await": awaitNode("await", "pick", "core.primitive.integer"),
			"sink":  {ID: "sink", Op: runtime.OpVariableSet, Config: map[string]json.RawMessage{"variable": json.RawMessage(`"x"`)}},
		},
		Entrypoints: map[string]runtime.ExecEntry{
			"fire": {Kind: runtime.EntryOnCall, Node: "fire", Target: runtime.ExecTarget{Node: "sink"}},
			"arm":  {Kind: runtime.EntryOnStart, Target: runtime.ExecTarget{Node: "await"}},
		},
	}
	show.LoadExec(cockpitActiveID, activeGraph, activeBundle, activeProg)
	if err := show.SetActive(cockpitActiveID, nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	// --- stream-level rule: one on-call trigger, scope stream ------------
	ruleGraph := &compiler.Graph{SceneID: cockpitRuleID, SceneVersion: "sha256:r"}
	ruleProg := &runtime.ExecProgram{
		BlueprintKey: "rule",
		Nodes:        map[string]*runtime.ExecNode{"sink": {ID: "sink", Op: runtime.OpVariableSet, Config: map[string]json.RawMessage{"variable": json.RawMessage(`"y"`)}}},
		Entrypoints: map[string]runtime.ExecEntry{
			"toggle": {Kind: runtime.EntryOnCall, Node: "toggle", Target: runtime.ExecTarget{Node: "sink"}},
		},
	}
	if err := show.PromoteStreamRule(cockpitRuleID, ruleGraph, &compiler.RenderBundle{SceneVersion: "sha256:r"}, ruleProg); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}

	// --- dormant scene: must be excluded ---------------------------------
	ghostGraph := &compiler.Graph{SceneID: cockpitOtherID, SceneVersion: "sha256:g"}
	ghostProg := &runtime.ExecProgram{
		BlueprintKey: "ghost",
		Nodes:        map[string]*runtime.ExecNode{"sink": {ID: "sink", Op: runtime.OpVariableSet, Config: map[string]json.RawMessage{"variable": json.RawMessage(`"z"`)}}},
		Entrypoints: map[string]runtime.ExecEntry{
			"never": {Kind: runtime.EntryOnCall, Node: "never", Target: runtime.ExecTarget{Node: "sink"}},
		},
	}
	show.LoadExec(cockpitOtherID, ghostGraph, &compiler.RenderBundle{SceneVersion: "sha256:g"}, ghostProg)

	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: testLogger(), Metrics: m, Show: show})
	return &cockpitFixture{mux: mux, show: show}
}

func getContracts(t *testing.T, f *cockpitFixture, role, query string) (*httptest.ResponseRecorder, cockpitContracts) {
	t.Helper()
	w := opRequest(t, f.mux, "GET", "/api/v1/cockpit/contracts"+query, role, nil)
	var body cockpitContracts
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v (raw=%s)", err, w.Body.String())
		}
	}
	return w, body
}

func TestCockpit_RequiresOperatorRole(t *testing.T) {
	f := newCockpitFixture(t)
	w := opRequest(t, f.mux, "GET", "/api/v1/cockpit/contracts?stream_id=s1", "viewer", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer: got %d, want 403", w.Code)
	}
}

func TestCockpit_StreamIDRequired(t *testing.T) {
	f := newCockpitFixture(t)
	w := opRequest(t, f.mux, "GET", "/api/v1/cockpit/contracts", "operator", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing stream_id: got %d, want 400", w.Code)
	}
}

func TestCockpit_AggregatesParamsAndTriggers(t *testing.T) {
	f := newCockpitFixture(t)
	w, body := getContracts(t, f, "operator", "?stream_id=s1")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if body.StreamID != "s1" {
		t.Fatalf("stream_id echoed = %q, want s1", body.StreamID)
	}
	// One param from the active scene.
	if len(body.Params) != 1 || body.Params[0].Path != "__inputs.bp.title" || body.Params[0].Scope != scopeScene {
		t.Fatalf("params = %+v, want one scene-scoped __inputs.bp.title", body.Params)
	}
	// Triggers: active bp/fire (scene) + rule/toggle (stream). Ghost EXCLUDED.
	gotTrig := map[string]string{} // "bp_key/entry" -> scope
	for _, tr := range body.Triggers {
		gotTrig[tr.BlueprintKey+"/"+tr.EntrypointID] = tr.Scope
		if tr.State != "armed" {
			t.Fatalf("trigger %s state = %q, want armed", tr.EntrypointID, tr.State)
		}
	}
	if gotTrig["bp/fire"] != scopeScene {
		t.Fatalf("bp/fire scope = %q, want scene (triggers=%+v)", gotTrig["bp/fire"], body.Triggers)
	}
	if gotTrig["rule/toggle"] != scopeStream {
		t.Fatalf("rule/toggle scope = %q, want stream", gotTrig["rule/toggle"])
	}
	if _, ok := gotTrig["ghost/never"]; ok {
		t.Fatalf("dormant ghost trigger leaked into contract: %+v", body.Triggers)
	}
}

func TestCockpit_PendingAwaitPresentAndScoped(t *testing.T) {
	f := newCockpitFixture(t)
	// The active scene's on-start arms the await; poll until it surfaces.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, body := getContracts(t, f, "operator", "?stream_id=s1")
		if len(body.Awaits) == 1 {
			a := body.Awaits[0]
			if a.BlueprintKey != "bp" || a.AwaitName != "pick" {
				t.Fatalf("await = %+v, want bp/pick", a)
			}
			if a.ValueType != "core.primitive.integer" {
				t.Fatalf("await value_type = %q", a.ValueType)
			}
			if a.State != "armed" {
				t.Fatalf("await state = %q, want armed", a.State)
			}
			if a.Scope != scopeScene {
				t.Fatalf("await scope = %q, want scene", a.Scope)
			}
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatal("pending await never surfaced in cockpit contract")
}
