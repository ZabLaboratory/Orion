package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// These tests exercise the real push handler end-to-end over a pure-Go SQLite
// store (no Postgres, no cgo) so they run in the standard test job — proving
// the switch fixes (idempotence + on-air swap guard) actually execute, not
// just compile. The PG-backed twins live in tests/e2e (CI e2e job).

// switchStubFetcher serves one layout + one blueprint and counts layout
// fetches — the proxy for "a compile happened".
type switchStubFetcher struct {
	layoutCalls int32
}

func (f *switchStubFetcher) FetchCanvasLayout(_ context.Context, v string) (*compiler.CanvasLayout, error) {
	atomic.AddInt32(&f.layoutCalls, 1)
	return &compiler.CanvasLayout{Version: v, Root: compiler.LayoutNode{Kind: "stack", ID: "root"}}, nil
}

func (f *switchStubFetcher) FetchBlueprint(_ context.Context, id string) (*compiler.BlueprintGraph, error) {
	return &compiler.BlueprintGraph{ID: id, Nodes: []compiler.BlueprintNode{
		{ID: "out.x", Compute: "core.input", Config: map[string]json.RawMessage{"name": json.RawMessage(`"score"`)}},
	}}, nil
}

func (f *switchStubFetcher) FetchBlueprintGraph(_ context.Context, _ string, _ int) (*compiler.ResolvedBlueprintGraph, error) {
	return nil, compiler.ErrRefUnresolved
}

func (f *switchStubFetcher) FetchComponent(_ context.Context, _ compiler.ComponentRef) (*compiler.UserComponent, error) {
	return nil, compiler.ErrRefUnresolved
}

func (f *switchStubFetcher) FetchComputeManifest(_ context.Context) (compiler.ComputeManifest, error) {
	return compiler.ComputeManifest{"core.input": {IsPure: true, IsBounded: true, Version: "1"}}, nil
}

func switchTestServer(t *testing.T, fetcher compiler.Fetcher) (*httptest.Server, *runtime.Show, store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "switch.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(st.Close)

	logger := obs.NewLogger(config.Config{LogLevel: "error", LogFormat: config.LogFormatText})
	registry := runtime.NewComputeRegistry()
	show := runtime.NewShow(registry, logger)
	t.Cleanup(show.Stop)
	testMgr := runtime.NewTestSessionManager(registry, logger, time.Minute)
	t.Cleanup(testMgr.Close)
	harness := runtime.NewHarness(registry, logger, runtime.DefaultValidationBudget)

	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger:  logger,
		Metrics: obs.NewMetrics(),
		Config:  config.Config{PushTimeout: 10 * time.Second, ValidationTimeout: 30 * time.Second},
		Show:    show,
		Test:    testMgr,
		Store:   st,
		Fetcher: fetcher,
		Harness: harness,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, show, st
}

func switchPost(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
	req.Header.Set("X-Authenticated-User", "op-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func switchWaitValidated(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/validation")
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var out map[string]any
			_ = json.Unmarshal(raw, &out)
			if resp.StatusCode == 200 && out["status"] == "validated" {
				return
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("validation never reached validated for %s", base)
}

// TestSwitchFix_IdenticalRePush_SkipsCompile (task 1): a byte-identical
// re-push skips Compile and every upstream fetch (zero extra layout fetch).
func TestSwitchFix_IdenticalRePush_SkipsCompile(t *testing.T) {
	fetcher := &switchStubFetcher{}
	srv, _, st := switchTestServer(t, fetcher)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "idem"); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()
	body := `{"canvas_version":"v1","blue_blueprint_id":"bp-1"}`

	code1, b1 := switchPost(t, base+"/push", body)
	if code1 != http.StatusOK {
		t.Fatalf("first push = %d %v", code1, b1)
	}
	if n := atomic.LoadInt32(&fetcher.layoutCalls); n != 1 {
		t.Fatalf("first push made %d layout fetches, want 1", n)
	}

	code2, b2 := switchPost(t, base+"/push", body)
	if code2 != http.StatusOK {
		t.Fatalf("re-push = %d %v", code2, b2)
	}
	if n := atomic.LoadInt32(&fetcher.layoutCalls); n != 1 {
		t.Fatalf("identical re-push RE-COMPILED (layout fetches = %d, want 1) — idempotence broken", n)
	}
	if b2["idempotent"] != true {
		t.Fatalf("re-push not flagged idempotent: %v", b2)
	}
	if b1["scene_version"] != b2["scene_version"] {
		t.Fatalf("idempotent re-push minted a different version: %v vs %v", b1["scene_version"], b2["scene_version"])
	}
}

// TestSwitchFix_RePushActiveScene_NoReload (task 3): an identical re-push of
// the LIVE scene is a silent no-op — the runtime keeps the SAME scene
// instance (no LoadExec), so the antenna never reloads.
func TestSwitchFix_RePushActiveScene_NoReload(t *testing.T) {
	fetcher := &switchStubFetcher{}
	srv, show, st := switchTestServer(t, fetcher)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "guard"); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()
	body := `{"canvas_version":"v1","blue_blueprint_id":"bp-1"}`

	if code, b := switchPost(t, base+"/push", body); code != http.StatusOK {
		t.Fatalf("push = %d %v", code, b)
	}
	if code, _ := switchPost(t, base+"/validate", `{}`); code != http.StatusAccepted {
		t.Fatalf("validate not accepted")
	}
	switchWaitValidated(t, base)
	if code, b := switchPost(t, srv.URL+"/api/v1/show/active-scene", `{"scene_id":"`+sceneID.String()+`"}`); code != http.StatusOK {
		t.Fatalf("activate = %d %v", code, b)
	}

	before := show.Active()
	if before == nil || before.ID() != sceneID.String() {
		t.Fatalf("scene not active before re-push")
	}

	code, resp := switchPost(t, base+"/push", body)
	if code != http.StatusOK {
		t.Fatalf("identical re-push = %d %v", code, resp)
	}
	if after := show.Active(); before != after {
		t.Fatalf("identical re-push of the LIVE scene reloaded the antenna: scene instance swapped (guard failed)")
	}
}
