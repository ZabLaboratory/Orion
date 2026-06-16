package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Tests for the service-scoped simulate endpoint (ADR 015, issues
// #194/#195/#196/#197). Two axes:
//
//   - the requireServiceScope gate (calqued on the exact-membership rule of
//     #86 / gate_probe_test.go): role=service AND the EXACT scope
//     `orion.validate.session` passes; a parent/wildcard/superstring/absent
//     scope or any non-service role is a frank 403 FORBIDDEN (fail-closed, R3);
//   - the endpoint body handling: a valid draft graph → 200 ValidationReport,
//     a corrupt graph → 400 INVALID_GRAPH, an over-bound body → 413.
//
// Plus a regression-guard: the /show/* operator surface is untouched — the
// simulate gate rejects an operator token (role≠service), proving simulate
// rides a separate gate, not the operator path (R2).

// simulateDeps wires a no-store harness (the simulate path never touches the
// DB) — the same shape probeHarnessDeps builds.
func simulateDeps() PublicDeps {
	reg := runtime.NewComputeRegistry()
	logger := probeLogger()
	return PublicDeps{
		Logger:  logger,
		Harness: runtime.NewHarness(reg, logger, runtime.ValidationBudget{MaxSteps: 50_000, MaxWall: 0}),
	}
}

// simulateRequest builds a POST /api/v1/validate/simulate request carrying
// the given body, with the trust headers ZabGate would inject for a service
// token holding `scopes`.
func simulateRequest(role string, scopes []string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/validate/simulate", bytes.NewReader(body))
	if role != "" {
		r.Header.Set("X-Authenticated-User", "svc-bluemcp")
		r.Header.Set("X-Authenticated-Role", role)
	}
	if len(scopes) > 0 {
		r.Header.Set("X-Authenticated-Paths", strings.Join(scopes, " "))
	}
	return r
}

// execIn/execOut/dataIn mirror the authoring port shapes Blue seeds
// (stdlib_seeder): the exec discriminator on the spine pins, `data` on value
// pins. Same helpers the compiler round-trip test uses.
func execIn(n string) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
}
func execOut(n string) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
}
func dataPort(n string) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: n, Type: "any", Kind: "data"}
}

// simulateGraphBody marshals a simulate request body around a valid
// AUTHORING-level Blue graph (a compiler.BlueprintGraph the endpoint compiles
// in-body, NOT a pre-built compiler.Graph — that is the #198 coverage hole
// this rewrite closes, ADR 015 A1.5) and a chat synthetic event.
func simulateGraphBody(t *testing.T) []byte {
	t.Helper()
	return simulateBodyFor(t, simulateValidBlueprint(), runtime.SyntheticEvent{Topic: "chat"})
}

// simulateBodyFor marshals a request body around an authoring graph + event.
func simulateBodyFor(t *testing.T, bp *compiler.BlueprintGraph, ev runtime.SyntheticEvent) []byte {
	t.Helper()
	graphRaw, err := json.Marshal(bp)
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	body, err := json.Marshal(simulateBody{Graph: graphRaw, SyntheticEvent: ev})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return body
}

// simulateValidBlueprint is an authoring-level Blue graph as create_draft_version
// emits: an on-start firing a variable.set, and an on-event(chat) firing a
// second variable.set — two entrypoints the harness must fire and report. It
// is a BlueprintGraph ({nodes, edges, variables}), never an ExecProgram built
// in Go (R7: the test MUST exercise the in-body compilation path).
func simulateValidBlueprint() *compiler.BlueprintGraph {
	return &compiler.BlueprintGraph{
		ID: "sim-valid",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{execOut("then")}},
			{ID: "lit", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`1`)},
				Outputs: []compiler.BlueprintPort{dataPort("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"x"`)},
				Inputs:  []compiler.BlueprintPort{execIn("exec_in"), dataPort("value")},
				Outputs: []compiler.BlueprintPort{execOut("then")}},
			{ID: "onchat", Compute: "core.event.on-event@1",
				Config:  map[string]json.RawMessage{"event_name": json.RawMessage(`"chat"`)},
				Outputs: []compiler.BlueprintPort{execOut("then")}},
			{ID: "lit2", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`2`)},
				Outputs: []compiler.BlueprintPort{dataPort("out")}},
			{ID: "set2", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"y"`)},
				Inputs:  []compiler.BlueprintPort{execIn("exec_in"), dataPort("value")},
				Outputs: []compiler.BlueprintPort{execOut("then")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "lit", FromPort: "out", ToNode: "set", ToPort: "value"},
			{FromNode: "onchat", FromPort: "then", ToNode: "set2", ToPort: "exec_in"},
			{FromNode: "lit2", FromPort: "out", ToNode: "set2", ToPort: "value"},
		},
	}
}

// ---- gate: pass / fail axes ------------------------------------------------

func TestSimulateGate_ServiceExactScope_Passes(t *testing.T) {
	body := simulateGraphBody(t)
	r := simulateRequest("service", []string{simulateScope}, body)
	w := httptest.NewRecorder()

	postSimulate(simulateDeps())(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("service + exact scope: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestSimulateGate_Rejects covers every fail-closed shape with a frank 403:
// the parent scope, a wildcard, a superstring, an absent scope, and a
// non-service role (operator) carrying the exact scope (R3 — role gate too).
func TestSimulateGate_Rejects(t *testing.T) {
	body := simulateGraphBody(t)
	cases := []struct {
		name   string
		role   string
		scopes []string
	}{
		{"parent scope", "service", []string{"orion.validate"}},
		{"wildcard scope", "service", []string{"orion.*"}},
		{"validate wildcard", "service", []string{"orion.validate.*"}},
		{"superstring scope", "service", []string{"orion.validate.session.extra"}},
		{"absent scope", "service", []string{"orion.other"}},
		{"no scope header", "service", nil},
		{"operator role exact scope", "operator", []string{simulateScope}},
		{"admin role exact scope", "admin", []string{simulateScope}},
		{"no identity at all", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := simulateRequest(c.role, c.scopes, body)
			w := httptest.NewRecorder()

			postSimulate(simulateDeps())(w, r)

			if w.Code != http.StatusForbidden {
				t.Fatalf("%s: code = %d, want 403", c.name, w.Code)
			}
			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("%s: response not JSON: %v", c.name, err)
			}
			if resp["code"] != "FORBIDDEN" {
				t.Fatalf("%s: code = %q, want FORBIDDEN", c.name, resp["code"])
			}
		})
	}
}

// ---- endpoint: body handling -----------------------------------------------

func TestSimulate_ValidGraph_Returns200Report(t *testing.T) {
	body := simulateGraphBody(t)
	r := simulateRequest("service", []string{simulateScope}, body)
	w := httptest.NewRecorder()

	postSimulate(simulateDeps())(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var rep runtime.ValidationReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("response is not a ValidationReport: %v", err)
	}
	if rep.HarnessVersion != runtime.HarnessVersion {
		t.Fatalf("harness_version = %q, want %q", rep.HarnessVersion, runtime.HarnessVersion)
	}
	if rep.Status != runtime.StatusValidated {
		t.Fatalf("status = %s, want validated; body=%s", rep.Status, w.Body.String())
	}
	// on-start (always fired) + on-event(chat) (matches the synthetic topic)
	// → two EntrypointResults, both proven from the in-body-compiled program.
	if len(rep.Blueprints) != 1 || len(rep.Blueprints[0].Entrypoints) != 2 {
		t.Fatalf("unexpected report shape: %+v", rep)
	}
}

func TestSimulate_CorruptGraph_Returns400(t *testing.T) {
	// A body whose `graph` field is syntactically broken JSON — decode fails.
	body := []byte(`{"graph":{"scene_id":,},"synthetic_event":{"topic":"chat"}}`)
	r := simulateRequest("service", []string{simulateScope}, body)
	w := httptest.NewRecorder()

	postSimulate(simulateDeps())(w, r)

	// The outer body itself is invalid JSON here → INVALID_BODY 400. Use a
	// graph that decodes as a body but whose graph value is unparseable.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestSimulate_GraphNotAGraph_Returns400InvalidGraph(t *testing.T) {
	// Valid outer body, but `graph` is a JSON array — fails to decode into
	// compiler.Graph → 400 INVALID_GRAPH.
	body, err := json.Marshal(simulateBody{
		Graph:          json.RawMessage(`[1,2,3]`),
		SyntheticEvent: runtime.SyntheticEvent{Topic: "chat"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := simulateRequest("service", []string{simulateScope}, body)
	w := httptest.NewRecorder()

	postSimulate(simulateDeps())(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["code"] != "INVALID_GRAPH" {
		t.Fatalf("code = %q, want INVALID_GRAPH", resp["code"])
	}
}

func TestSimulate_BodyOverBound_Returns413(t *testing.T) {
	// A body larger than maxSimulateBody (1 MiB) is refused before any decode.
	big := bytes.Repeat([]byte("a"), int(maxSimulateBody)+1)
	r := simulateRequest("service", []string{simulateScope}, big)
	w := httptest.NewRecorder()

	postSimulate(simulateDeps())(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["code"] != "BODY_TOO_LARGE" {
		t.Fatalf("code = %q, want BODY_TOO_LARGE", resp["code"])
	}
}

// ---- regression-guard: /show/* operator surface untouched ------------------

// TestSimulate_DoesNotRideOperatorGate proves the simulate route is gated by
// requireServiceScope, NOT by the operator path: an operator identity (the
// shape that passes /show/* operator endpoints) is rejected 403 by simulate.
// Combined with TestSimulateGate_ServiceExactScope_Passes (service passes),
// this shows simulate has its own gate distinct from /show/* (ADR 015 R2).
func TestSimulate_DoesNotRideOperatorGate(t *testing.T) {
	body := simulateGraphBody(t)
	r := simulateRequest("operator", nil, body)
	w := httptest.NewRecorder()

	postSimulate(simulateDeps())(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("operator on simulate: code = %d, want 403 (separate gate)", w.Code)
	}
}

// TestSimulate_RouteRegisteredDistinctFromShow confirms the route is wired
// under /validate/* and is reachable (the operator /show/active-scene route
// still resolves to its own handler — the mux keeps them separate).
func TestSimulate_RouteRegisteredDistinctFromShow(t *testing.T) {
	mux := http.NewServeMux()
	deps := simulateDeps()
	// Register only the two routes this guard inspects (full RegisterPublic
	// needs a WSServer/Store the no-store harness deps don't carry).
	mux.HandleFunc("POST /api/v1/validate/simulate", postSimulate(deps))
	mux.HandleFunc("POST /api/v1/show/active-scene", func(w http.ResponseWriter, _ *http.Request) {
		// Sentinel: a request to /show/active-scene must NOT land on simulate.
		w.WriteHeader(http.StatusTeapot)
	})

	// /show/active-scene resolves to the sentinel, not the simulate handler.
	rShow := httptest.NewRequest(http.MethodPost, "/api/v1/show/active-scene", nil)
	wShow := httptest.NewRecorder()
	mux.ServeHTTP(wShow, rShow)
	if wShow.Code != http.StatusTeapot {
		t.Fatalf("/show/active-scene resolved to wrong handler: code = %d", wShow.Code)
	}

	// /validate/simulate resolves to the simulate handler (403 without creds).
	rSim := httptest.NewRequest(http.MethodPost, "/api/v1/validate/simulate", nil)
	wSim := httptest.NewRecorder()
	mux.ServeHTTP(wSim, rSim)
	if wSim.Code != http.StatusForbidden {
		t.Fatalf("/validate/simulate did not reach the gated handler: code = %d", wSim.Code)
	}
}

// ============================================================================
// R7 — compile in-body end-to-end (ADR 015 Amendment 1 §A1.5, issue #199).
//
// The #198 tests built ExecPrograms directly in Go, so they never exercised
// the decode of an authoring-level Blue graph — the exact path that produced
// blueprints:null in prod (A1.1). These tests submit a REAL BlueprintGraph
// ({nodes, edges, variables}) JSON and prove the endpoint compiles it in-body
// (no Fetcher, no egress) into a non-null per-entrypoint report, and that a
// graph the compiler rejects returns 400 COMPILE_FAILED — distinct from the
// 400 INVALID_GRAPH of a malformed body.
// ============================================================================

// simulateOK posts an authoring graph through the gated handler and returns
// the decoded report, failing the test on a non-200.
func simulateOK(t *testing.T, bp *compiler.BlueprintGraph, ev runtime.SyntheticEvent) runtime.ValidationReport {
	t.Helper()
	body := simulateBodyFor(t, bp, ev)
	r := simulateRequest("service", []string{simulateScope}, body)
	w := httptest.NewRecorder()
	postSimulate(simulateDeps())(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var rep runtime.ValidationReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("response is not a ValidationReport: %v", err)
	}
	return rep
}

// simulateCompileFail posts an authoring graph and asserts a 400 whose code is
// COMPILE_FAILED (never INVALID_GRAPH, never a 200 blueprints:null), returning
// the diagnostics list.
func simulateCompileFail(t *testing.T, bp *compiler.BlueprintGraph) []any {
	t.Helper()
	body := simulateBodyFor(t, bp, runtime.SyntheticEvent{Topic: "chat"})
	r := simulateRequest("service", []string{simulateScope}, body)
	w := httptest.NewRecorder()
	postSimulate(simulateDeps())(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["code"] != "COMPILE_FAILED" {
		t.Fatalf("code = %v, want COMPILE_FAILED (distinct from INVALID_GRAPH); body=%s", resp["code"], w.Body.String())
	}
	diags, _ := resp["diagnostics"].([]any)
	if len(diags) == 0 {
		t.Fatalf("COMPILE_FAILED carried no diagnostics; body=%s", w.Body.String())
	}
	return diags
}

// TestSimulateR7_BlueprintGraph_BlueprintsNonNull is the headline R7: a real
// authoring-level Blue graph (on-start + on-event(chat)) compiled in-body
// yields a report whose blueprints is NON-NULL with one EntrypointResult per
// fired entrypoint, each carrying populated proof fields. This is exactly the
// path #198 never tested — the bug was blueprints:null on this very shape.
func TestSimulateR7_BlueprintGraph_BlueprintsNonNull(t *testing.T) {
	rep := simulateOK(t, simulateValidBlueprint(), runtime.SyntheticEvent{Topic: "chat"})

	if rep.Status != runtime.StatusValidated {
		t.Fatalf("status = %s, want validated", rep.Status)
	}
	// THE regression assertion: blueprints is non-null and populated.
	if len(rep.Blueprints) != 1 {
		t.Fatalf("blueprints null/empty — the #199 bug; report = %+v", rep)
	}
	eps := rep.Blueprints[0].Entrypoints
	// on-start (always) + on-event(chat) (topic match) → two results.
	if len(eps) != 2 {
		t.Fatalf("want 2 entrypoints fired, got %d: %+v", len(eps), eps)
	}
	kinds := map[string]bool{}
	for _, er := range eps {
		kinds[er.Kind] = true
		if !er.Pass {
			t.Fatalf("entrypoint %s failed: %+v", er.Entrypoint, er)
		}
		// Each fired entry's variable.set wrote its __vars leaf — proof the
		// in-body-compiled exec spine actually executed (steps/leaves peopled).
		if len(er.LeavesWritten) == 0 {
			t.Fatalf("entrypoint %s wrote no leaves — exec spine did not run: %+v", er.Entrypoint, er)
		}
		if len(er.NodesCovered) == 0 {
			t.Fatalf("entrypoint %s covered no exec nodes: %+v", er.Entrypoint, er)
		}
	}
	if !kinds["on-start"] || !kinds["on-event"] {
		t.Fatalf("expected both an on-start and an on-event entrypoint, got kinds=%v", kinds)
	}
}

// TestSimulateR7_UnknownDefinition_CompileFailed: an exec node whose
// definition maps to no runtime op → 400 COMPILE_FAILED (EXEC_OP_UNMAPPED),
// never blueprints:null.
func TestSimulateR7_UnknownDefinition_CompileFailed(t *testing.T) {
	bp := &compiler.BlueprintGraph{
		ID: "bad-def",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{execOut("then")}},
			// Carries an exec pin (→ exec node) but an unknown definition.
			{ID: "mystery", Compute: "core.does.not.exist@9",
				Inputs:  []compiler.BlueprintPort{execIn("exec_in")},
				Outputs: []compiler.BlueprintPort{execOut("then")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "mystery", ToPort: "exec_in"},
		},
	}
	simulateCompileFail(t, bp)
}

// TestSimulateR7_DanglingExecTarget_CompileFailed: an on-start whose exec out
// targets a node absent from the exec table → 400 COMPILE_FAILED
// (EXEC_UNKNOWN_NODE).
func TestSimulateR7_DanglingExecTarget_CompileFailed(t *testing.T) {
	bp := &compiler.BlueprintGraph{
		ID: "dangling",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{execOut("then")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"x"`)},
				Inputs:  []compiler.BlueprintPort{execIn("exec_in")},
				Outputs: []compiler.BlueprintPort{execOut("then")}},
		},
		// start's exec out points at "ghost" — not an exec node in the table.
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "ghost", ToPort: "exec_in"},
		},
	}
	diags := simulateCompileFail(t, bp)
	found := false
	for _, d := range diags {
		if m, ok := d.(map[string]any); ok && m["code"] == "EXEC_UNKNOWN_NODE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an EXEC_UNKNOWN_NODE diagnostic, got %+v", diags)
	}
}

// TestSimulateR7_ReferenceNode_CompileFailed: a `reference` node (ADR 014
// expansion needs a Fetcher → egress) is descoped MVP and rejected
// REFERENCE_NOT_SUPPORTED under COMPILE_FAILED — the invariant that keeps the
// endpoint zero-egress (A1.4).
func TestSimulateR7_ReferenceNode_CompileFailed(t *testing.T) {
	bp := &compiler.BlueprintGraph{
		ID: "has-ref",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{execOut("then")}},
			{ID: "callee", Reference: &compiler.BlueprintReference{
				BlueprintID: "00000000-0000-0000-0000-000000000000", Version: 1}},
		},
	}
	diags := simulateCompileFail(t, bp)
	found := false
	for _, d := range diags {
		if m, ok := d.(map[string]any); ok && m["code"] == "REFERENCE_NOT_SUPPORTED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a REFERENCE_NOT_SUPPORTED diagnostic, got %+v", diags)
	}
}

// TestSimulateR7_EmptyNodes_InvalidGraphNotCompileFailed pins the code split:
// a body whose graph has NO nodes is a malformed body → 400 INVALID_GRAPH,
// NOT COMPILE_FAILED (which is reserved for a well-formed graph the compiler
// rejects). This guards the §A1.5 distinctness clause.
func TestSimulateR7_EmptyNodes_InvalidGraphNotCompileFailed(t *testing.T) {
	body := simulateBodyFor(t, &compiler.BlueprintGraph{ID: "empty"}, runtime.SyntheticEvent{Topic: "chat"})
	r := simulateRequest("service", []string{simulateScope}, body)
	w := httptest.NewRecorder()
	postSimulate(simulateDeps())(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["code"] != "INVALID_GRAPH" {
		t.Fatalf("empty-nodes graph: code = %q, want INVALID_GRAPH (not COMPILE_FAILED)", resp["code"])
	}
}

// TestSimulateR7_Isolation_HTTPRequestNoEgress is R4 at the endpoint level: a
// graph whose compiled exec spine fires core.http.request@1 runs in the
// validation-mode clone (inert effect seam) — it completes WITHOUT real
// egress, and the attempted call is surfaced in effects_attempted. Mirrors the
// runtime B10 no-egress proof (TestHarness_ValidationModeNoEffect) but through
// the in-body compile + endpoint, proving isolation survives the new path.
func TestSimulateR7_Isolation_HTTPRequestNoEgress(t *testing.T) {
	bp := &compiler.BlueprintGraph{
		ID: "iso-http",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{execOut("then")}},
			{ID: "url", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`"https://example.invalid/should-never-be-dialed"`)},
				Outputs: []compiler.BlueprintPort{dataPort("out")}},
			{ID: "http", Compute: "core.http.request@1",
				Inputs:  []compiler.BlueprintPort{execIn("exec_in"), dataPort("url")},
				Outputs: []compiler.BlueprintPort{execOut("then"), execOut("error")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "http", ToPort: "exec_in"},
			{FromNode: "url", FromPort: "out", ToNode: "http", ToPort: "url"},
		},
	}
	rep := simulateOK(t, bp, runtime.SyntheticEvent{Topic: "chat"})
	if len(rep.Blueprints) != 1 || len(rep.Blueprints[0].Entrypoints) != 1 {
		t.Fatalf("unexpected report shape: %+v", rep)
	}
	er := rep.Blueprints[0].Entrypoints[0]
	if !er.Pass {
		t.Fatalf("http.request entrypoint failed (validation-mode should complete via inert seam): %+v", er)
	}
	// The attempted egress is recorded — the op reached the seam — yet no real
	// socket was dialed (example.invalid would fail; the test passing proves
	// the inert synthetic response drove the chain, not a live dial).
	found := false
	for _, ea := range er.EffectsTried {
		if ea.Op == runtime.OpHTTPRequest {
			found = true
		}
	}
	if !found {
		t.Fatalf("effects_attempted does not list the http.request (inert seam not exercised): %+v", er.EffectsTried)
	}
}
