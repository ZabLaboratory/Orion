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

// TestCockpit_StreamItemsCarryRuleID (ADR 009 Amendment 1, #285): every
// stream-scope facet item is stamped with rule_id = the promoted rule's stable
// id (the streamRules key). Scene-scope items carry no rule_id.
func TestCockpit_StreamItemsCarryRuleID(t *testing.T) {
	f := newCockpitFixture(t)
	w, body := getContracts(t, f, "operator", "?stream_id=s1")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var found bool
	for _, tr := range body.Triggers {
		switch tr.Scope {
		case scopeStream:
			if tr.RuleID != cockpitRuleID {
				t.Fatalf("stream trigger %s rule_id = %q, want %q", tr.EntrypointID, tr.RuleID, cockpitRuleID)
			}
			found = true
		case scopeScene:
			if tr.RuleID != "" {
				t.Fatalf("scene trigger %s leaked rule_id %q", tr.EntrypointID, tr.RuleID)
			}
		}
	}
	if !found {
		t.Fatalf("no stream-scope trigger found (triggers=%+v)", body.Triggers)
	}
	for _, p := range body.Params {
		if p.Scope == scopeScene && p.RuleID != "" {
			t.Fatalf("scene param %s leaked rule_id %q", p.Path, p.RuleID)
		}
	}
}

// TestCockpit_MultiRuleDistinctRuleIDs (#285): with TWO promoted stream rules,
// each item routes uniquely by its own rule_id — the exact ambiguity #286
// resolves (both may compile with blueprint_id "_").
func TestCockpit_MultiRuleDistinctRuleIDs(t *testing.T) {
	f := newCockpitFixture(t)
	const secondRuleID = "55555555-5555-5555-5555-555555555555"
	secondGraph := &compiler.Graph{SceneID: secondRuleID, SceneVersion: "sha256:r2"}
	secondProg := &runtime.ExecProgram{
		BlueprintKey: "rule2",
		Nodes:        map[string]*runtime.ExecNode{"sink": {ID: "sink", Op: runtime.OpVariableSet, Config: map[string]json.RawMessage{"variable": json.RawMessage(`"w"`)}}},
		Entrypoints: map[string]runtime.ExecEntry{
			"toggle": {Kind: runtime.EntryOnCall, Node: "toggle", Target: runtime.ExecTarget{Node: "sink"}},
		},
	}
	if err := f.show.PromoteStreamRule(secondRuleID, secondGraph, &compiler.RenderBundle{SceneVersion: "sha256:r2"}, secondProg); err != nil {
		t.Fatalf("PromoteStreamRule(second): %v", err)
	}

	w, body := getContracts(t, f, "operator", "?stream_id=s1")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	byRule := map[string]string{} // rule_id -> blueprint_key/entry
	for _, tr := range body.Triggers {
		if tr.Scope != scopeStream {
			continue
		}
		if tr.RuleID == "" {
			t.Fatalf("stream trigger %s has empty rule_id", tr.EntrypointID)
		}
		byRule[tr.RuleID] = tr.BlueprintKey + "/" + tr.EntrypointID
	}
	if byRule[cockpitRuleID] != "rule/toggle" {
		t.Fatalf("rule %s -> %q, want rule/toggle (byRule=%+v)", cockpitRuleID, byRule[cockpitRuleID], byRule)
	}
	if byRule[secondRuleID] != "rule2/toggle" {
		t.Fatalf("rule %s -> %q, want rule2/toggle (byRule=%+v)", secondRuleID, byRule[secondRuleID], byRule)
	}
}

// TestCockpit_SceneItemShapeByteStable (RC1, #285): the raw JSON of every
// scene-scope facet item has NO `rule_id` key — the additive field must not
// perturb the frozen Conduit contract (#213). Stream items DO carry the key.
func TestCockpit_SceneItemShapeByteStable(t *testing.T) {
	f := newCockpitFixture(t)
	w := opRequest(t, f.mux, "GET", "/api/v1/cockpit/contracts?stream_id=s1", "operator", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var raw struct {
		Params   []map[string]json.RawMessage `json:"params"`
		Triggers []map[string]json.RawMessage `json:"triggers"`
		Awaits   []map[string]json.RawMessage `json:"awaits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	check := func(kind string, items []map[string]json.RawMessage) {
		for _, it := range items {
			_, hasRuleID := it["rule_id"]
			isStream := string(it["scope"]) == `"`+scopeStream+`"`
			if isStream && !hasRuleID {
				t.Fatalf("%s stream item missing rule_id: %v", kind, it)
			}
			if !isStream && hasRuleID {
				t.Fatalf("%s scene item carries rule_id (RC1 violated): %v", kind, it)
			}
		}
	}
	check("param", raw.Params)
	check("trigger", raw.Triggers)
	check("await", raw.Awaits)
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

// TestCockpit_OverlayAppTriggerStreamScoped (ADR 016 Prism §3.2, issue #283,
// RC3): an overlay-app is driven from a STREAM-LEVEL rule whose on-call spine
// runs `core.overlay-app.set@1`. The cockpit contract must surface that on-call
// trigger with scope `stream` — the operator button the reveal/hide rides. The
// overlay node itself carries no operator surface; the trigger is the generic
// on-call, exposed by the existing #209 aggregation (no overlay-specific code).
func TestCockpit_OverlayAppTriggerStreamScoped(t *testing.T) {
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	show.SetExecMetrics(m)
	t.Cleanup(show.Stop)

	// A stream-level rule: on-call `overlay-toggle` → overlay-app.set (reveal).
	ruleGraph := &compiler.Graph{SceneID: "overlay-rule", SceneVersion: "sha256:ov"}
	ruleProg := &runtime.ExecProgram{
		BlueprintKey: "overlay",
		Nodes: map[string]*runtime.ExecNode{
			"set": {
				ID: "set", Op: runtime.OpOverlayAppSet,
				Config: map[string]json.RawMessage{
					"app_id": json.RawMessage(`"app-1"`),
					"on_air": json.RawMessage(`true`),
				},
			},
		},
		Entrypoints: map[string]runtime.ExecEntry{
			"overlay-toggle": {Kind: runtime.EntryOnCall, Node: "overlay-toggle", Target: runtime.ExecTarget{Node: "set"}},
		},
	}
	if err := show.PromoteStreamRule("overlay-rule", ruleGraph, &compiler.RenderBundle{SceneVersion: "sha256:ov"}, ruleProg); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}

	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: testLogger(), Metrics: m, Show: show})
	f := &cockpitFixture{mux: mux, show: show}

	w, body := getContracts(t, f, "operator", "?stream_id=s1")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var scope string
	found := false
	for _, tr := range body.Triggers {
		if tr.BlueprintKey == "overlay" && tr.EntrypointID == "overlay-toggle" {
			scope, found = tr.Scope, true
		}
	}
	if !found {
		t.Fatalf("overlay/overlay-toggle trigger absent (triggers=%+v)", body.Triggers)
	}
	if scope != scopeStream {
		t.Fatalf("overlay-toggle scope = %q, want stream", scope)
	}
}
