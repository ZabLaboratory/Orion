package compiler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestFetchCanvasLayout_OldKeyEntryIsMiss proves the contract-version fix: a
// cache entry written under the LEGACY key ("layout:"+version, with no contract
// prefix) is NOT hallucinated as a hit after the constant was introduced. The
// key changed, so the stale entry is a MISS and the layout is re-fetched from
// the network — the exact staleness bug ZabCanvas #150 exposed.
func TestFetchCanvasLayout_OldKeyEntryIsMiss(t *testing.T) {
	const version = "sha256:v1"
	// The bytes the OLD adapter serialised and cached under the legacy key —
	// what a client cache holds on the field after the server-side #150 change.
	stale := `{"version":"sha256:v1","root":{"kind":"frame","id":"root"},"defaults":{"__lit.text.a":"STALE"}}`
	// What the server serves NOW (post-#150 adaptation) for the same version.
	fresh := `{"version":"sha256:v1","root":{"kind":"frame","id":"root"},"defaults":{"__lit.text.a":"FRESH"}}`

	dir := t.TempDir()
	// Seed a legacy-keyed entry, exactly as a pre-fix Orion would have left it.
	legacy := fetchCache{dir: dir}
	legacy.put("layout:"+version, []byte(stale))

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fresh))
	}))
	defer srv.Close()

	f := NewHTTPFetcher(srv.URL, srv.URL, "").WithCacheDir(dir)
	got, err := f.FetchCanvasLayout(context.Background(), version)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("legacy-keyed cache entry was served as a hit; the contract-version fix must treat it as a MISS and re-fetch")
	}
	if v, ok := got.Defaults["__lit.text.a"]; !ok || string(v) != `"FRESH"` {
		t.Fatalf("served stale bytes instead of the re-fetched fresh layout: defaults=%v", got.Defaults)
	}
}

// TestFetchCanvasLayout_NewKeyRoundTrips proves the fix does not cost the normal
// path its cache: two successive fetches of the same version under the NEW
// contract key re-use the cached entry — the second issues zero HTTP.
func TestFetchCanvasLayout_NewKeyRoundTrips(t *testing.T) {
	const version = "sha256:v9"
	layout := `{"version":"sha256:v9","root":{"kind":"frame","id":"root"},"defaults":{"__lit.text.a":"HI"}}`

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(layout))
	}))
	defer srv.Close()

	f := NewHTTPFetcher(srv.URL, srv.URL, "").WithCacheDir(t.TempDir())
	for i := 0; i < 2; i++ {
		if _, err := f.FetchCanvasLayout(context.Background(), version); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("same version under the new key should serve the second fetch from cache; HTTP hits = %d, want 1", got)
	}
}
