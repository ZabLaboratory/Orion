package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/providers"
)

// Tests for POST /api/v1/validate/program (C2, ADR-BLUE-012 R6 §6.3): the
// contre-validation of an ALREADY-COMPILED blue.program.v1 against Engine B
// (internal/bluehost + internal/providers), as opposed to /validate/simulate
// (validate_simulate_test.go), which dry-runs a draft AUTHORING graph
// through Engine A. Two axes, same shape as simulate's own test file:
//
//   - the requireServiceScope gate, calqued on simulate's own (exact
//     membership of `orion.validate.program`, a distinct scope);
//   - the verdict itself: a program declaring a capability
//     internal/providers.Registry() serves is accepted (servable=true); one
//     declaring a capability nothing in the registry serves is refused
//     (servable=false, code=CAPABILITY_UNAVAILABLE) — the exact class of
//     failure a clean compile can never catch, and the reason this route
//     exists at all (Refs #181).

// validateProgramDeps wires the real Zab provider registry — the whole
// point of this route is contre-validating against it, not a stub.
func validateProgramDeps() SceneIntentDeps {
	return SceneIntentDeps{
		Providers: providers.Registry(),
		Policy:    providers.Policy(true),
	}
}

// validateProgramHTTPRequest builds a POST /api/v1/validate/program request
// carrying body, with the trust headers ZabGate would inject for a service
// token holding scopes — mirrors simulateRequest (validate_simulate_test.go)
// for the distinct URL/scope this route uses.
func validateProgramHTTPRequest(role string, scopes []string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/validate/program", bytes.NewReader(body))
	if role != "" {
		r.Header.Set("X-Authenticated-User", "svc-zabcanvas")
		r.Header.Set("X-Authenticated-Role", role)
	}
	if len(scopes) > 0 {
		r.Header.Set("X-Authenticated-Paths", strings.Join(scopes, " "))
	}
	return r
}

func validateProgramBody(t *testing.T, program json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(validateProgramRequest{Program: program})
	if err != nil {
		t.Fatalf("marshal validateProgramRequest: %v", err)
	}
	return body
}

// unknownCapabilityProgram takes the same 02-http-requires.program.json
// fixture httpRequiresProgram(t) reads (scene_intent_providers_test.go) and
// renames the ONE capability its requires/effects entries declare to
// something internal/providers.Registry() does not serve. Both entries are
// renamed together so the fixture's own requires/effects cross-reference
// (blueruntime's effectTypesMatch) stays internally consistent — only the
// capability identity changes. program_digest IS content-verified by
// blueruntime.ParseProgram (PROGRAM_DIGEST_MISMATCH otherwise), so it is
// recomputed with internal/canonical, the same shared LSML canonicalization
// blueruntime verifies on receipt.
func unknownCapabilityProgram(t *testing.T) json.RawMessage {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(httpRequiresProgram(t)))
	decoder.UseNumber() // canonical.Bytes requires json.Number, not float64
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	requires, _ := doc["requires"].([]any)
	effects, _ := doc["effects"].([]any)
	if len(requires) != 1 || len(effects) != 1 {
		t.Fatalf("fixture shape drifted: requires=%d effects=%d", len(requires), len(effects))
	}
	requires[0].(map[string]any)["capability"] = "core.unknown-capability"
	effects[0].(map[string]any)["capability"] = "core.unknown-capability"
	delete(doc, "program_digest")
	digest, err := canonical.Digest(doc)
	if err != nil {
		t.Fatalf("recompute program_digest: %v", err)
	}
	doc["program_digest"] = digest
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal mutated fixture: %v", err)
	}
	return out
}

// TestPostValidateProgram_RequiresExactServiceScope proves the gate: only
// role=service AND the exact scope `orion.validate.program` reaches the body
// handler. A parent/wildcard scope, the wrong role, or no auth at all is a
// frank 403 — same fail-closed posture as simulateScope.
func TestPostValidateProgram_RequiresExactServiceScope(t *testing.T) {
	body := validateProgramBody(t, httpRequiresProgram(t))
	cases := []struct {
		name   string
		role   string
		scopes []string
	}{
		{"no auth headers", "", nil},
		{"operator role, exact scope", "operator", []string{validateProgramScope}},
		{"admin role, exact scope", "admin", []string{validateProgramScope}},
		{"service role, wrong scope", "service", []string{"orion.validate.session"}},
		{"service role, parent scope", "service", []string{"orion.validate"}},
		{"service role, wildcard scope", "service", []string{"orion.*"}},
		{"service role, no scope", "service", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validateProgramHTTPRequest(tc.role, tc.scopes, body)
			rec := httptest.NewRecorder()
			postValidateProgram(validateProgramDeps())(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPostValidateProgram_AcceptsServableProgram(t *testing.T) {
	body := validateProgramBody(t, httpRequiresProgram(t))
	r := validateProgramHTTPRequest("service", []string{validateProgramScope}, body)
	rec := httptest.NewRecorder()
	postValidateProgram(validateProgramDeps())(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp validateProgramResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Servable {
		t.Fatalf("expected servable=true, got %+v", resp)
	}
	if resp.Code != "" || resp.Reason != "" {
		t.Fatalf("expected no code/reason on a servable verdict, got %+v", resp)
	}
}

// TestPostValidateProgram_RefusesUnknownCapability is the route's whole
// reason to exist (Refs #181): a program that would compile cleanly still
// declares a capability nothing in internal/providers.Registry() serves, and
// the route must say so precisely — never a bare/default "ok".
func TestPostValidateProgram_RefusesUnknownCapability(t *testing.T) {
	body := validateProgramBody(t, unknownCapabilityProgram(t))
	r := validateProgramHTTPRequest("service", []string{validateProgramScope}, body)
	rec := httptest.NewRecorder()
	postValidateProgram(validateProgramDeps())(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (verdict, not a protocol error), got %d: %s", rec.Code, rec.Body.String())
	}
	var resp validateProgramResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Servable {
		t.Fatalf("expected servable=false for an unserved capability, got %+v", resp)
	}
	if resp.Code != "CAPABILITY_UNAVAILABLE" {
		t.Fatalf("expected code=CAPABILITY_UNAVAILABLE, got %+v", resp)
	}
	if resp.Reason == "" {
		t.Fatalf("expected a non-empty reason, got %+v", resp)
	}
}

func TestPostValidateProgram_InvalidBody(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"malformed JSON", []byte("{not json")},
		{"missing program field", []byte(`{}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validateProgramHTTPRequest("service", []string{validateProgramScope}, tc.body)
			rec := httptest.NewRecorder()
			postValidateProgram(validateProgramDeps())(rec, r)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPostValidateProgram_BodyTooLarge(t *testing.T) {
	big := append([]byte(`{"program":`), bytes.Repeat([]byte("0"), maxValidateProgramBody+1)...)
	big = append(big, '}')
	r := validateProgramHTTPRequest("service", []string{validateProgramScope}, big)
	rec := httptest.NewRecorder()
	postValidateProgram(validateProgramDeps())(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}
