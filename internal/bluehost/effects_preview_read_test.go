package bluehost

import (
	"net/http"
	"net/http/httptest"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

func TestEffectHandlers_PreviewExecutesReadOnlyHTTP(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"match_id":"lec-1","home":"Team A"}`))
	}))
	defer srv.Close()
	u := srv.URL
	// The private-address exception is test-only; production still uses the
	// configured allowlist and DNS/IP re-checks.
	egress := effects.NewEgressPolicy([]string{"127.0.0.1"}, true).InsecureAllowPrivateForTest()
	handler := NewEffectHandlers(EffectDeps{Egress: egress}, blueruntime.Preview)["core.http.request@1"]

	outputs, err := handler(nil, map[string]any{"url": u, "method": http.MethodGet})
	if err != nil {
		t.Fatalf("preview GET: %v", err)
	}
	if hits != 1 {
		t.Fatalf("preview GET hits = %d, want 1", hits)
	}
	if outputs["ok"] != true {
		t.Fatalf("preview GET outputs = %#v", outputs)
	}
}

func TestEffectHandlers_PreviewExecutesCuratedReadRoute(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"match_id":"lec-1","home":"Team A"}`))
	}))
	defer srv.Close()
	var gotPaths []string
	client := effects.NewServiceCallClient(srv.URL, func(paths []string) string {
		gotPaths = append([]string(nil), paths...)
		return "ephemeral-preview-token"
	}, nil)
	resolver := func(service, routeID string) (ServiceCallRoute, bool) {
		if service != "truth" || routeID != "leaguepedia.preview" {
			return ServiceCallRoute{}, false
		}
		return ServiceCallRoute{
			Service: "truth", RouteID: routeID, Method: http.MethodGet,
			PathTemplate: "/truth/leaguepedia/matches/{match_id}/preview",
			Params: []string{"match_id"}, TokenPaths: []string{"query.read.truth"},
		}, true
	}
	handler := NewEffectHandlers(EffectDeps{
		ServiceCall: client, ResolveServiceRoute: resolver,
	}, blueruntime.Preview)["core.service.call@1"]

	outputs, err := handler(map[string]any{
		"__route": map[string]any{"service": "truth", "route_id": "leaguepedia.preview"},
	}, map[string]any{"params": map[string]any{"match_id": "LEC/2026/M1"}})
	if err != nil {
		t.Fatalf("preview service.call: %v", err)
	}
	if gotPath != "/truth/leaguepedia/matches/LEC%2F2026%2FM1/preview" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer ephemeral-preview-token" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if len(gotPaths) != 1 || gotPaths[0] != "query.read.truth" {
		t.Fatalf("token paths = %#v", gotPaths)
	}
	if outputs["ok"] != true {
		t.Fatalf("outputs = %#v", outputs)
	}
}
