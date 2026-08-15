package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Operator runtime surface tests (Orion #209, Blue ADR 008 §3.2/§3.3) at
// the HTTP boundary: authz, active-only 409, on-call fire, pending listing,
// resolve type-check, and the invariant-7 late-resolve 410.

const opSceneID = "11111111-1111-1111-1111-111111111111"

// operatorFixture builds a Show with one scene hosting blueprint key "bp"
// with an on-call entrypoint ("call") and an await-value node ("pick"). The
// scene is made active so the operator routes (active-only) reach it.
type operatorFixture struct {
	mux  *http.ServeMux
	show *runtime.Show
}

func newOperatorFixture(t *testing.T) *operatorFixture {
	t.Helper()
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	show.SetExecMetrics(m)
	t.Cleanup(show.Stop)

	graph := &compiler.Graph{
		SceneID: opSceneID, SceneVersion: "sha256:op",
		Defaults: map[string]json.RawMessage{"__vars.bp.called": json.RawMessage(`null`)},
	}
	// on-call → variable.set(called = payload). await → variable.set(picked = value).
	prog := &runtime.ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*runtime.ExecNode{
			"set.called": {ID: "set.called", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{"variable": json.RawMessage(`"called"`)},
				Data:   []runtime.ExecDataInput{{Port: "value", From: "call", FromPort: "payload"}}},
			"pick": {ID: "pick", Op: runtime.OpOperatorAwait,
				Config: map[string]json.RawMessage{
					"await_name": json.RawMessage(`"pick"`),
					"value_type": json.RawMessage(`"core.primitive.integer"`)},
				Next: map[string]runtime.ExecTarget{"then": {Node: "set.picked"}}},
			"set.picked": {ID: "set.picked", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{"variable": json.RawMessage(`"picked"`)},
				Data:   []runtime.ExecDataInput{{Port: "value", From: "pick", FromPort: "value"}}},
		},
		Entrypoints: map[string]runtime.ExecEntry{
			"call": {Kind: runtime.EntryOnCall, Node: "call", Target: runtime.ExecTarget{Node: "set.called"}},
			// on-start arms the await as soon as the scene goes live.
			"arm": {Kind: runtime.EntryOnStart, Target: runtime.ExecTarget{Node: "pick"}},
		},
	}
	show.LoadExec(opSceneID, graph, &compiler.RenderBundle{SceneVersion: "sha256:op"}, prog)
	if err := show.SetActive(opSceneID, nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: testLogger(), Metrics: m, Show: show})
	return &operatorFixture{mux: mux, show: show}
}

func opRequest(t *testing.T, mux *http.ServeMux, method, path, role string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, &buf)
	if role != "" {
		r.Header.Set("X-Authenticated-User", "op-user")
		r.Header.Set("X-Authenticated-Role", role)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestOperator_CallRequiresOperatorRole(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/bp/call", "viewer",
		map[string]any{"payload": 1})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer call: got %d, want 403", w.Code)
	}
}

func TestOperator_CallDormantBlueprintIs409(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/other-bp/call", "operator",
		map[string]any{"payload": 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("dormant blueprint call: got %d, want 409", w.Code)
	}
}

func TestOperator_CallUnknownEntrypointIs409(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/bp/nope", "operator",
		map[string]any{"payload": 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("unknown entrypoint: got %d, want 409", w.Code)
	}
}

// TestOperator_CallFiresWithPayload proves the antenna leg of POST
// /operator/call reaches a REAL Engine B on-air instance
// (ORION-OPERATOR-RAIL-ENGINE-B, #335) — the plain default path (no
// ?target=, no ?rule=) no longer routes to Show.Active() (Show's roster has
// had no production populator since #331), it routes to bluehost.Host.
// Addressed via the default token "_" here; TestOperator_CallEngineB
// _RealBlueprintIDIsAcceptedButIgnored below proves a real (non-default)
// blueprint_id — what Prism actually sends — works identically.
func TestOperator_CallFiresWithPayload(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "call", "called", "", "", "")
	f := newEngineBOperatorFixture(t, program)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/call", "operator",
		map[string]any{"payload": "hello-operator"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("call: got %d, want 202 (body=%s)", w.Code, w.Body.String())
	}
	if got, _ := f.peekVar(t, "called").(string); got != "hello-operator" {
		t.Fatalf("called = %#v, want %q", f.peekVar(t, "called"), "hello-operator")
	}
}

// TestOperator_CallEngineB_RealBlueprintIDIsAcceptedButIgnored pins the
// fix for the regression team-lead caught: Prism sends a REAL, non-default
// blueprint_id on every operator/call (cockpit-api.ts builds
// `/operator/call/${blueprintId}/${entrypointId}` from actual binding data,
// e.g. composite-tree-stage.tsx's `binding.blueprint_id` — never "" or "_").
// Gating the antenna leg on blueprint_id == "" would 409 every real Prism
// button. Verified against Blue's own compiler
// (blue_engine/program/compiler.py:1000-1011): on-call ids are flat and
// globally unique per compiled program, never blueprint-key-namespaced, so
// blueprint_id has no routing role to play — only entrypoint_id does. This
// asserts an ARBITRARY non-default blueprint_id still fires correctly.
func TestOperator_CallEngineB_RealBlueprintIDIsAcceptedButIgnored(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "on_lck_arm", "called", "", "", "")
	f := newEngineBOperatorFixture(t, program)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/lck-scoreboard-v2/on_lck_arm", "operator",
		map[string]any{"payload": "region-lck"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("call with real blueprint_id: got %d, want 202 (body=%s)", w.Code, w.Body.String())
	}
	if got, _ := f.peekVar(t, "called").(string); got != "region-lck" {
		t.Fatalf("called = %#v, want %q", f.peekVar(t, "called"), "region-lck")
	}

	// An entrypoint truly absent from the served program still fails closed,
	// whatever blueprint_id rides along with it — no safety was traded away.
	wUnknown := opRequest(t, f.mux, "POST", "/api/v1/operator/call/lck-scoreboard-v2/nope", "operator",
		map[string]any{"payload": 1})
	if wUnknown.Code != http.StatusConflict {
		t.Fatalf("unknown entrypoint with real blueprint_id: got %d, want 409 (body=%s)",
			wUnknown.Code, wUnknown.Body.String())
	}
}

func TestOperator_PendingListsArmedAwait(t *testing.T) {
	f := newOperatorFixture(t)
	// The on-start arm parks the await; poll the pending route until listed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w := opRequest(t, f.mux, "GET", "/api/v1/runtime/bp/pending", "operator", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("pending: got %d, want 200", w.Code)
		}
		var resp struct {
			Pending []runtime.PendingAwait `json:"pending"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Pending) == 1 && resp.Pending[0].AwaitName == "pick" {
			if resp.Pending[0].ValueType != "core.primitive.integer" {
				t.Fatalf("value_type = %q", resp.Pending[0].ValueType)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("await never listed in pending")
}

// TestOperator_ResolveTypeMismatchIs422 and TestOperator_ResolveValidResumes
// prove the antenna leg of POST /operator/resolve reaches a REAL Engine B
// on-air instance — same rationale as TestOperator_CallFiresWithPayload.
// blueruntime's own Resolve already fails closed on an unparked/unknown
// await name (EVENT_MALFORMED, mapped to 410 AWAIT_GONE by
// postOperatorResolveEngineB) — these two prove the type-check (Host's own
// pre-check, AWAIT_TYPE_MISMATCH -> 422) and the success path.
func TestOperator_ResolveTypeMismatchIs422(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "call", "called", "pick", "core.primitive.integer", "picked")
	f := newEngineBOperatorFixture(t, program)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/_/pick", "operator",
		map[string]any{"value": 3.5})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("type mismatch: got %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
}

func TestOperator_ResolveValidResumes(t *testing.T) {
	program := buildEngineBOperatorProgram(t, "call", "called", "pick", "core.primitive.integer", "picked")
	f := newEngineBOperatorFixture(t, program)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/_/pick", "operator",
		map[string]any{"value": 42})
	if w.Code != http.StatusOK {
		t.Fatalf("resolve: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if got, _ := f.peekVar(t, "picked").(float64); got != 42 {
		t.Fatalf("picked = %#v, want 42", f.peekVar(t, "picked"))
	}
}

func TestOperator_LateResolveAfterSwitchAwayIs410(t *testing.T) {
	f := newOperatorFixture(t)
	waitAwaitArmed(t, f)
	// Switch away: load + activate a second scene; the await of the first is
	// invalidated (ADR 008 invariant 7).
	const other = "22222222-2222-2222-2222-222222222222"
	f.show.LoadExec(other,
		&compiler.Graph{SceneID: other, SceneVersion: "sha256:op2"},
		&compiler.RenderBundle{SceneVersion: "sha256:op2"})
	if err := f.show.SetActive(other, nil); err != nil {
		t.Fatalf("SetActive other: %v", err)
	}
	// bp is no longer active → 410 (dormant blueprint).
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/bp/pick", "operator",
		map[string]any{"value": 1})
	if w.Code != http.StatusGone {
		t.Fatalf("late resolve after switch-away: got %d, want 410 (body=%s)", w.Code, w.Body.String())
	}
}

// --- ?rule={rule_id} selector (ADR 009 Amendment 1, #286) -----------------

const opRuleID = "99999999-9999-9999-9999-999999999999"

// promoteOpRule adds a stream-level rule (blueprint "rule") to the fixture's
// show, hosting an on-call entrypoint "toggle" that writes its payload to
// __vars.rule.hit. Returns the rule id (its addressing token for ?rule=).
func promoteOpRule(t *testing.T, f *operatorFixture) string {
	t.Helper()
	ruleGraph := &compiler.Graph{
		SceneID: opRuleID, SceneVersion: "sha256:rule",
		Defaults: map[string]json.RawMessage{"__vars.rule.hit": json.RawMessage(`null`)},
	}
	ruleProg := &runtime.ExecProgram{
		BlueprintKey: "rule",
		Nodes: map[string]*runtime.ExecNode{
			"set.hit": {ID: "set.hit", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{"variable": json.RawMessage(`"hit"`)},
				Data:   []runtime.ExecDataInput{{Port: "value", From: "toggle", FromPort: "payload"}}},
		},
		Entrypoints: map[string]runtime.ExecEntry{
			"toggle": {Kind: runtime.EntryOnCall, Node: "toggle", Target: runtime.ExecTarget{Node: "set.hit"}},
		},
	}
	if err := f.show.PromoteStreamRule(opRuleID, ruleGraph, &compiler.RenderBundle{SceneVersion: "sha256:rule"}, ruleProg); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}
	return opRuleID
}

// TestOperator_CallRuleSelectorFires (#286): ?rule={rule_id} routes a call to a
// promoted stream-level rule (NOT the active scene). The rule's blueprint "rule"
// is dormant on the active-scene path — only the selector reaches it.
func TestOperator_CallRuleSelectorFires(t *testing.T) {
	f := newOperatorFixture(t)
	ruleID := promoteOpRule(t, f)

	// Without the selector, "rule" is not part of the active scene → 409.
	wDormant := opRequest(t, f.mux, "POST", "/api/v1/operator/call/rule/toggle", "operator",
		map[string]any{"payload": 1})
	if wDormant.Code != http.StatusConflict {
		t.Fatalf("rule call without selector: got %d, want 409", wDormant.Code)
	}

	// With ?rule=, it routes to the promoted rule and fires.
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/rule/toggle?rule="+ruleID, "operator",
		map[string]any{"payload": map[string]any{"n": 5}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("rule call: got %d, want 202 (body=%s)", w.Code, w.Body.String())
	}
	// The effect landed on the RULE scene's state, not the active scene.
	rc, err := f.show.Get(ruleID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sub, snap := rc.Subscribe(16)
		v, ok := snap.State["__vars.rule.hit"]
		sub.Close()
		if ok && string(v) == `{"n":5}` {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("rule on-call effect never landed on the rule scene")
}

// TestOperator_RuleAndPreviewSelectorsConflictIs400 (#286, RC3): ?rule and
// ?target=preview are mutually exclusive — no silent precedence.
func TestOperator_RuleAndPreviewSelectorsConflictIs400(t *testing.T) {
	f := newOperatorFixture(t)
	ruleID := promoteOpRule(t, f)
	q := "?rule=" + ruleID + "&target=preview"
	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/operator/call/rule/toggle" + q, map[string]any{"payload": 1}},
		{http.MethodPost, "/api/v1/operator/resolve/rule/pick" + q, map[string]any{"value": 1}},
		{http.MethodGet, "/api/v1/runtime/rule/pending" + q, nil},
	}
	for _, c := range cases {
		w := opRequest(t, f.mux, c.method, c.path, "operator", c.body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: got %d, want 400 (body=%s)", c.method, c.path, w.Code, w.Body.String())
		}
		var resp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Error != "SELECTOR_CONFLICT" {
			t.Fatalf("%s: error = %q, want SELECTOR_CONFLICT", c.path, resp.Error)
		}
	}
}

// TestOperator_DemotedRuleSelectorIs409RuleNotActive (#286, RC4 — Vigil
// explicitly required this test): ?rule={id} for an id that is not a currently
// promoted rule (here: demoted after promotion) is 409 RULE_NOT_ACTIVE, on all
// three routes.
func TestOperator_DemotedRuleSelectorIs409RuleNotActive(t *testing.T) {
	f := newOperatorFixture(t)
	ruleID := promoteOpRule(t, f)
	f.show.DemoteStreamRule(ruleID)

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/operator/call/rule/toggle?rule=" + ruleID, map[string]any{"payload": 1}},
		{http.MethodPost, "/api/v1/operator/resolve/rule/pick?rule=" + ruleID, map[string]any{"value": 1}},
		{http.MethodGet, "/api/v1/runtime/rule/pending?rule=" + ruleID, nil},
	}
	for _, c := range cases {
		w := opRequest(t, f.mux, c.method, c.path, "operator", c.body)
		if w.Code != http.StatusConflict {
			t.Fatalf("%s %s: got %d, want 409 (body=%s)", c.method, c.path, w.Code, w.Body.String())
		}
		var resp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Error != "RULE_NOT_ACTIVE" {
			t.Fatalf("%s %s: error = %q, want RULE_NOT_ACTIVE", c.method, c.path, resp.Error)
		}
	}

	// An id that was never a rule is likewise RULE_NOT_ACTIVE.
	w := opRequest(t, f.mux, http.MethodGet,
		"/api/v1/runtime/rule/pending?rule=deadbeef-0000-0000-0000-000000000000", "operator", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("never-promoted rule: got %d, want 409", w.Code)
	}
}

// waitAwaitArmed polls until the await is registered (on-start armed it).
func waitAwaitArmed(t *testing.T, f *operatorFixture) {
	t.Helper()
	sc, err := f.show.Get(opSceneID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sc.PendingAwaits("bp")) == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("await never armed")
}
