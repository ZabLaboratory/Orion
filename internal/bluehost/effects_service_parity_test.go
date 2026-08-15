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

func serviceParityRouteReference() map[string]any {
	return map[string]any{"service": "example", "route_id": "example.echo"}
}

func serviceParityRouteResolver(service, routeID string) (ServiceCallRoute, bool) {
	if service != "example" || routeID != "example.echo" {
		return ServiceCallRoute{}, false
	}
	return ServiceCallRoute{
		Service:      service,
		RouteID:      routeID,
		Method:       http.MethodPost,
		PathTemplate: "/svc/{id}",
		Params:       []string{"id"},
		TokenPaths:   []string{"svc.write"},
	}, true
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
	handler := NewEffectHandlers(EffectDeps{ServiceCall: client, ResolveServiceRoute: serviceParityRouteResolver}, blueruntime.Execute)["core.service.call@1"]
	outputs, err := handler(map[string]any{"__route": serviceParityRouteReference()}, serviceParityInputs("a/b"))
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

func TestEffectHandlers_ServiceCallReferenceWithoutResolverFailsClosed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := effects.NewServiceCallClient(srv.URL, func([]string) string { return "scoped" }, nil)
	handler := NewEffectHandlers(EffectDeps{ServiceCall: client}, blueruntime.Execute)["core.service.call@1"]
	if _, err := handler(map[string]any{"__route": serviceParityRouteReference()}, serviceParityInputs("alice")); err == nil || err.Error() != "EGRESS_ROUTE_UNRESOLVED: example/example.echo" {
		t.Fatalf("missing route resolver error=%v, want fail-closed EGRESS_ROUTE_UNRESOLVED", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("unresolved route emitted %d requests, want zero", got)
	}
}

func TestEffectHandlers_ServiceCallRejectsAuthoredTransportDetails(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := effects.NewServiceCallClient(srv.URL, func([]string) string { return "scoped" }, nil)
	handler := NewEffectHandlers(EffectDeps{ServiceCall: client, ResolveServiceRoute: serviceParityRouteResolver}, blueruntime.Execute)["core.service.call@1"]
	authoredDetails := map[string]any{
		"service":       "example",
		"route_id":      "example.echo",
		"method":        http.MethodPost,
		"path_template": "/svc/{id}",
		"params":        []string{"id"},
		"token_paths":   []string{"svc.write"},
	}
	if _, err := handler(map[string]any{"__route": authoredDetails}, serviceParityInputs("alice")); err == nil || err.Error() != "EGRESS_ROUTE_INVALID" {
		t.Fatalf("authored transport details error=%v, want EGRESS_ROUTE_INVALID", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("authored transport details emitted %d requests, want zero", got)
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
	noTokenHandler := NewEffectHandlers(EffectDeps{ServiceCall: noToken, ResolveServiceRoute: serviceParityRouteResolver}, blueruntime.Execute)["core.service.call@1"]
	if _, err := noTokenHandler(map[string]any{"__route": serviceParityRouteReference()}, serviceParityInputs("alice")); err == nil || err.Error() != "SERVICE_CALL_FAILED: no scoped egress token for [svc.write]" {
		t.Fatalf("missing token error=%v, want fail-closed SERVICE_CALL_FAILED", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("missing token emitted %d requests, want zero", got)
	}

	client := effects.NewServiceCallClient(srv.URL, func([]string) string { return "scoped" }, nil)
	invalidInputHandler := NewEffectHandlers(EffectDeps{ServiceCall: client, ResolveServiceRoute: serviceParityRouteResolver}, blueruntime.Execute)["core.service.call@1"]
	if _, err := invalidInputHandler(map[string]any{"__route": serviceParityRouteReference()}, map[string]any{
		"params":  map[string]any{},
		"payload": map[string]any{"source": "orion-358"},
	}); err == nil || len(err.Error()) < len("EGRESS_PARAM_MISSING") || err.Error()[:len("EGRESS_PARAM_MISSING")] != "EGRESS_PARAM_MISSING" {
		t.Fatalf("missing param error=%v, want EGRESS_PARAM_MISSING", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("missing param emitted %d requests, want zero", got)
	}

	handler := NewEffectHandlers(EffectDeps{
		ServiceCall:         client,
		ResolveServiceRoute: serviceParityRouteResolver,
		EgressBudget:        effects.NewStreamEgressLimiter(1, 60),
	}, blueruntime.Execute)["core.service.call@1"]
	if _, err := handler(map[string]any{"__route": serviceParityRouteReference()}, serviceParityInputs("alice")); err != nil {
		t.Fatalf("first budgeted service.call: %v", err)
	}
	if _, err := handler(map[string]any{"__route": serviceParityRouteReference()}, serviceParityInputs("bob")); err == nil || err.Error() != "EGRESS_BUDGET_EXCEEDED" {
		t.Fatalf("second budgeted service.call error=%v, want EGRESS_BUDGET_EXCEEDED", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("budgeted service.call emitted %d requests, want one", got)
	}
}
