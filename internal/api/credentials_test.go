package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/auth"
)

// authedRequest builds a request that passes the requireOperator gate.
func authedRequest(method, path, accountID string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.SetPathValue("id", accountID)
	r.Header.Set("X-Authenticated-User", "op-1")
	r.Header.Set("X-Authenticated-Role", "operator")
	return r
}

func TestStreamKey_503WhenQuasarNotConfigured(t *testing.T) {
	deps := PublicDeps{}
	w := httptest.NewRecorder()
	getStreamKey(deps)(w, authedRequest("GET", "/api/v1/credentials/abc/stream-key", "abc"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "QUASAR_NOT_WIRED") {
		t.Fatalf("body = %s, want QUASAR_NOT_WIRED", w.Body.String())
	}
}

func TestStreamKey_403WhenNotOperator(t *testing.T) {
	deps := PublicDeps{
		QuasarBaseURL: "http://example.invalid/quasar",
		ServiceTokens: &auth.ServiceTokenManager{StaticToken: "x"},
	}
	r := httptest.NewRequest("GET", "/api/v1/credentials/abc/stream-key", nil)
	r.SetPathValue("id", "abc")
	// no X-Authenticated-* headers — anonymous
	w := httptest.NewRecorder()
	getStreamKey(deps)(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestStreamKey_ProxiesUpstream200(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer st-abc" {
			t.Errorf("upstream got Authorization=%q, want Bearer st-abc", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v1/credentials/acc-7/stream-key" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"account_id":"acc-7","stream_key":"live_xxx"}`))
	}))
	defer upstream.Close()

	deps := PublicDeps{
		QuasarBaseURL: upstream.URL,
		ServiceTokens: &auth.ServiceTokenManager{StaticToken: "st-abc"},
	}
	w := httptest.NewRecorder()
	getStreamKey(deps)(w, authedRequest("GET", "/api/v1/credentials/acc-7/stream-key", "acc-7"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), `"stream_key":"live_xxx"`) {
		t.Fatalf("body = %s", string(body))
	}
}

func TestStreamKey_ForwardsUpstream404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"stream key unset"}`))
	}))
	defer upstream.Close()

	deps := PublicDeps{
		QuasarBaseURL: upstream.URL,
		ServiceTokens: &auth.ServiceTokenManager{StaticToken: "x"},
	}
	w := httptest.NewRecorder()
	getStreamKey(deps)(w, authedRequest("GET", "/api/v1/credentials/x/stream-key", "x"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestStreamKey_502WhenUpstreamUnreachable(t *testing.T) {
	deps := PublicDeps{
		// Port 1 is reserved on linux + immediately rejects on Win → either way the dial fails.
		QuasarBaseURL: "http://127.0.0.1:1",
		ServiceTokens: &auth.ServiceTokenManager{StaticToken: "x"},
	}
	w := httptest.NewRecorder()
	getStreamKey(deps)(w, authedRequest("GET", "/api/v1/credentials/x/stream-key", "x"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "QUASAR_UNREACHABLE") {
		t.Fatalf("body = %s, want QUASAR_UNREACHABLE", w.Body.String())
	}
}
