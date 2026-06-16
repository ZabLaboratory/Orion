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

// simulateGraphBody marshals a simulate request body around a valid draft
// graph (one terminating on-start program) and a chat synthetic event.
func simulateGraphBody(t *testing.T) []byte {
	t.Helper()
	graph := simulateValidGraph()
	graphRaw, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	body, err := json.Marshal(simulateBody{
		Graph:          graphRaw,
		SyntheticEvent: runtime.SyntheticEvent{Topic: "chat"},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return body
}

// simulateValidGraph is a compiler.Graph carrying one well-formed exec
// program whose on-start sets a variable — it terminates within budget.
func simulateValidGraph() *compiler.Graph {
	prog := &runtime.ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*runtime.ExecNode{
			"set": {
				ID: "set", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{
					"name":  json.RawMessage(`"x"`),
					"value": json.RawMessage(`1`),
				},
			},
		},
		Entrypoints: map[string]runtime.ExecEntry{
			"start": {Kind: runtime.EntryOnStart, Target: runtime.ExecTarget{Node: "set"}},
		},
	}
	rawProg, err := json.Marshal(prog)
	if err != nil {
		panic(err)
	}
	return &compiler.Graph{
		SceneID:      "sim-valid",
		SceneVersion: "sha256:sim-valid",
		ExecPrograms: []json.RawMessage{rawProg},
		Defaults:     map[string]json.RawMessage{},
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
	if len(rep.Blueprints) != 1 || len(rep.Blueprints[0].Entrypoints) != 1 {
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
