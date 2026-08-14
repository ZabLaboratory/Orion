package bluehost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/effects"
)

func serviceParityRoute() map[string]any {
	return map[string]any{
		"method":        http.MethodPost,
		"path_template": "/svc/{id}",
		"params":        []string{"id"},
		"token_paths":   []string{"svc.write"},
	}
}

func serviceParityInputs(id string) map[string]any {
	return map[string]any{
		"params":  map[string]any{"id": id},
		"payload": map[string]any{"source": "orion-358"},
	}
}

func TestEffectHandlers_ServiceCallNon2xxPreservesStatusBodyAndOK(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"accepted":false,"reason":"conflict"}`)
	}))
	defer srv.Close()

	client := effects.NewServiceCallClient(srv.URL, func(paths []string) string {
		if len(paths) != 1 || paths[0] != "svc.write" {
			t.Errorf("service.call token paths=%v, want [svc.write]", paths)
		}
		return "scoped"
	}, nil)
	handler := NewEffectHandlers(EffectDeps{ServiceCall: client}, blueruntime.Execute)["core.service.call@1"]
	outputs, err := handler(map[string]any{"__route": serviceParityRoute()}, serviceParityInputs("a/b"))
	if err != nil {
		t.Fatalf("service.call non-2xx: %v", err)
	}
	if gotPath != "/svc/a%2Fb" {
		t.Fatalf("service.call path=%q, want escaped path", gotPath)
	}
	if string(gotBody) != `{"source":"orion-358"}` {
		t.Fatalf("service.call body=%s, want canonical payload", gotBody)
	}
	if status, ok := outputs["status"].(json.Number); !ok || status.String() != "409" {
		t.Fatalf("service.call status=%#v, want 409", outputs["status"])
	}
	if outputs["ok"] != false {
		t.Fatalf("service.call ok=%#v, want false for non-2xx", outputs["ok"])
	}
	body, ok := outputs["body"].(map[string]any)
	if !ok || body["accepted"] != false || body["reason"] != "conflict" {
		t.Fatalf("service.call body output=%#v, want decoded response", outputs["body"])
	}
}

func TestEffectHandlers_ServiceCallInvalidInputMissingTokenAndBudgetFailClosed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	noToken := effects.NewServiceCallClient(srv.URL, func([]string) string { return "" }, nil)
	noTokenHandler := NewEffectHandlers(EffectDeps{ServiceCall: noToken}, blueruntime.Execute)["core.service.call@1"]
	if _, err := noTokenHandler(map[string]any{"__route": serviceParityRoute()}, serviceParityInputs("alice")); err == nil || err.Error() != "SERVICE_CALL_FAILED: no scoped egress token for [svc.write]" {
		t.Fatalf("missing token error=%v, want fail-closed SERVICE_CALL_FAILED", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("missing token emitted %d requests, want zero", got)
	}

	client := effects.NewServiceCallClient(srv.URL, func([]string) string { return "scoped" }, nil)
	invalidInputHandler := NewEffectHandlers(EffectDeps{ServiceCall: client}, blueruntime.Execute)["core.service.call@1"]
	if _, err := invalidInputHandler(map[string]any{"__route": serviceParityRoute()}, map[string]any{
		"params":  map[string]any{},
		"payload": map[string]any{"source": "orion-358"},
	}); err == nil || len(err.Error()) < len("EGRESS_PARAM_MISSING") || err.Error()[:len("EGRESS_PARAM_MISSING")] != "EGRESS_PARAM_MISSING" {
		t.Fatalf("missing param error=%v, want EGRESS_PARAM_MISSING", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("missing param emitted %d requests, want zero", got)
	}

	handler := NewEffectHandlers(EffectDeps{
		ServiceCall:  client,
		EgressBudget: effects.NewStreamEgressLimiter(1, 60),
	}, blueruntime.Execute)["core.service.call@1"]
	if _, err := handler(map[string]any{"__route": serviceParityRoute()}, serviceParityInputs("alice")); err != nil {
		t.Fatalf("first budgeted service.call: %v", err)
	}
	if _, err := handler(map[string]any{"__route": serviceParityRoute()}, serviceParityInputs("bob")); err == nil || err.Error() != "EGRESS_BUDGET_EXCEEDED" {
		t.Fatalf("second budgeted service.call error=%v, want EGRESS_BUDGET_EXCEEDED", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("budgeted service.call emitted %d requests, want one", got)
	}
}
