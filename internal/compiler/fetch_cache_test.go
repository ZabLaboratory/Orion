package compiler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestEnvelopeFingerprint_StableAndDistinct proves the switch-fix idempotence
// key: an identical envelope fingerprints identically (so a re-push short-
// circuits), authored blueprint order is irrelevant (normalised), and any
// change to a compile input yields a different fingerprint (so a real change
// never aliases a stale artefact).
func TestEnvelopeFingerprint_StableAndDistinct(t *testing.T) {
	base := PushEnvelope{
		CanvasVersion: "sha256:aaa",
		Blueprints:    []BlueprintRef{{Key: "b", ID: "bp-2"}, {Key: "a", ID: "bp-1"}},
		Components:    []ComponentRef{{ID: "c1", Version: "v1"}},
	}
	fp1, err := EnvelopeFingerprint("scene-1", base)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	// Same inputs, blueprints in a different authored order → same fingerprint.
	reordered := base
	reordered.Blueprints = []BlueprintRef{{Key: "a", ID: "bp-1"}, {Key: "b", ID: "bp-2"}}
	fp2, err := EnvelopeFingerprint("scene-1", reordered)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("reordered blueprints changed the fingerprint: %s vs %s", fp1, fp2)
	}

	// Each of scene id, canvas version, a blueprint id, a component, and the
	// LSML hash must move the fingerprint.
	cases := map[string]func(*PushEnvelope, *string){
		"scene id":       func(_ *PushEnvelope, s *string) { *s = "scene-2" },
		"canvas version": func(e *PushEnvelope, _ *string) { e.CanvasVersion = "sha256:bbb" },
		"blueprint id": func(e *PushEnvelope, _ *string) {
			e.Blueprints = []BlueprintRef{{Key: "a", ID: "bp-9"}, {Key: "b", ID: "bp-2"}}
		},
		"component": func(e *PushEnvelope, _ *string) { e.Components = []ComponentRef{{ID: "c1", Version: "v2"}} },
		"lsml hash": func(e *PushEnvelope, _ *string) { e.LSMLBundleHash = "sha256:zzz" },
	}
	for name, mutate := range cases {
		env := base
		env.Blueprints = append([]BlueprintRef(nil), base.Blueprints...)
		env.Components = append([]ComponentRef(nil), base.Components...)
		scene := "scene-1"
		mutate(&env, &scene)
		got, err := EnvelopeFingerprint(scene, env)
		if err != nil {
			t.Fatalf("%s: fingerprint: %v", name, err)
		}
		if got == fp1 {
			t.Fatalf("%s change did not move the fingerprint", name)
		}
	}
}

// TestFetchCanvasLayout_DiskCache_SecondFetchNoHTTP proves the layout is
// content-addressed cached: a second fetch of the same version hits zero HTTP.
func TestFetchCanvasLayout_DiskCache_SecondFetchNoHTTP(t *testing.T) {
	var hits int32
	layout := `{"version":"sha256:v1","root":{"kind":"frame","id":"root"},"defaults":{"__lit.text.a":"HELLO"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if !strings.HasPrefix(r.URL.Path, "/api/v1/layouts/") {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(layout))
	}))
	defer srv.Close()

	f := NewHTTPFetcher(srv.URL, srv.URL, "").WithCacheDir(t.TempDir())

	first, err := f.FetchCanvasLayout(context.Background(), "sha256:v1")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if first.Version != "sha256:v1" || len(first.Defaults) != 1 {
		t.Fatalf("first fetch decoded wrong: %+v", first)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("first fetch made %d HTTP requests, want 1", got)
	}

	second, err := f.FetchCanvasLayout(context.Background(), "sha256:v1")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if second.Version != first.Version || len(second.Defaults) != 1 {
		t.Fatalf("cached fetch decoded wrong: %+v", second)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("second fetch of the same hash made HTTP requests (total %d), want the cache to serve it", got)
	}

	// A different version must miss and hit the network again.
	otherLayout := `{"version":"sha256:v2","root":{"kind":"frame","id":"root"},"defaults":{"__lit.text.a":"HI"}}`
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(otherLayout))
	})
	if _, err := f.FetchCanvasLayout(context.Background(), "sha256:v2"); err != nil {
		t.Fatalf("third fetch (new version): %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("a new version should have re-fetched; total HTTP = %d, want 2", got)
	}
}

// TestFetchCanvasLayout_NoCacheDir_AlwaysHitsNetwork proves a disabled cache
// (empty dir, the pre-cache behaviour) never suppresses a fetch.
func TestFetchCanvasLayout_NoCacheDir_AlwaysHitsNetwork(t *testing.T) {
	var hits int32
	layout := `{"version":"sha256:v1","root":{"kind":"frame","id":"root"},"defaults":{"__lit.text.a":"HELLO"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(layout))
	}))
	defer srv.Close()

	f := NewHTTPFetcher(srv.URL, srv.URL, "") // no WithCacheDir → disabled
	for i := 0; i < 2; i++ {
		if _, err := f.FetchCanvasLayout(context.Background(), "sha256:v1"); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("disabled cache should hit the network twice, got %d", got)
	}
}
