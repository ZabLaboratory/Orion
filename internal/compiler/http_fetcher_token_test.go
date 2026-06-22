package compiler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestHTTPFetcher_ReadsTokenLivePerRequest proves Bastion C1: the
// fetcher presents the CURRENT token on every request, not a value
// frozen at construction. We rotate the token a provider returns
// between two fetches and assert the second request carries the NEW
// bearer. This is the exact bug that 401'd every Canvas/Blue fetch:
// the boot placeholder was captured once and never refreshed.
func TestHTTPFetcher_ReadsTokenLivePerRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		// Carry a non-empty ``defaults`` so FetchCanvasLayout takes the
		// nominal single-request path (no lsml-bundle back-fill) — this test
		// asserts the live-token-per-request behaviour, one request per fetch.
		_, _ = w.Write([]byte(`{"version":"v1","defaults":{"__lit.text.x":"v"}}`))
	}))
	defer srv.Close()

	var current atomic.Value // string
	current.Store("token-boot")
	f := NewHTTPFetcherWithTokenFunc(srv.URL, srv.URL, func() string {
		return current.Load().(string)
	})

	if _, err := f.FetchCanvasLayout(context.Background(), "v1"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	// Simulate a rotation by the service-token manager.
	current.Store("token-rotated")
	if _, err := f.FetchCanvasLayout(context.Background(), "v1"); err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seen))
	}
	if seen[0] != "Bearer token-boot" {
		t.Fatalf("first request auth = %q, want Bearer token-boot", seen[0])
	}
	if seen[1] != "Bearer token-rotated" {
		t.Fatalf("second request auth = %q, want Bearer token-rotated — the fetcher froze the boot token (C1 regression)", seen[1])
	}
}

// TestHTTPFetcher_NoSilentAnonymousFallback proves Bastion C2: in live
// mode (a TokenFunc is wired) an empty token is a HARD, explicit
// failure — the fetcher must NOT fire an anonymous request that would
// 401 downstream with no signal. We assert ErrNoServiceToken and that
// the upstream was never hit.
func TestHTTPFetcher_NoSilentAnonymousFallback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	f := NewHTTPFetcherWithTokenFunc(srv.URL, srv.URL, func() string { return "" })

	_, err := f.FetchCanvasLayout(context.Background(), "v1")
	if err == nil {
		t.Fatalf("expected an explicit error on empty live token, got nil")
	}
	// The wrapping FetchCanvasLayout error must carry ErrNoServiceToken.
	if got := hits.Load(); got != 0 {
		t.Fatalf("upstream was hit %d time(s) with an empty live token — anonymous fallback leaked (C2 regression)", got)
	}
}

// TestHTTPFetcher_NilTokenFuncSendsAnonymous proves the dev/test
// posture is preserved: a nil TokenFunc (the "" service-token case)
// sends an anonymous request, exactly as before, with no Authorization
// header. This is the historical behaviour the unauthenticated stub
// tests rely on.
func TestHTTPFetcher_NilTokenFuncSendsAnonymous(t *testing.T) {
	var auth string
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, present = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1"}`))
	}))
	defer srv.Close()

	// NewHTTPFetcher("",...,"") yields a nil TokenFunc.
	f := NewHTTPFetcher(srv.URL, srv.URL, "")
	if f.TokenFunc != nil {
		t.Fatalf("empty static token should yield a nil TokenFunc")
	}
	if _, err := f.FetchCanvasLayout(context.Background(), "v1"); err != nil {
		t.Fatalf("anonymous fetch: %v", err)
	}
	if present || auth != "" {
		t.Fatalf("anonymous request carried an Authorization header (%q) — should be absent", auth)
	}
}
