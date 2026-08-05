package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"

	"net/http"
	"net/http/httptest"
)

// The wire half of the LSML_HASH_MISMATCH surfacing. The unit tests on
// persistLSMLAndMaybeAdopt prove the warning is PRODUCED; these prove it is
// SERVED — which is the whole point, since the mismatch already existed in
// Orion's log and the log is not a contract. Without the response carrying it,
// a producer cannot tell "your bundle drifted" from "this Orion runs bespoke
// mode", and ZabCanvas is forced to mirror the legacy mint in both cases
// (ZabCanvas docs/contracts/zabcanvas-orion-scene-version.md §3).

func lsmlWireServer(t *testing.T, mode config.LSDPMode) (*httptest.Server, store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "lsmlwire.db"))
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

	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger:  logger,
		Metrics: obs.NewMetrics(),
		Config: config.Config{
			PushTimeout:       10 * time.Second,
			ValidationTimeout: 30 * time.Second,
			LSDPMode:          mode,
		},
		Show:    show,
		Test:    testMgr,
		Store:   st,
		Fetcher: &switchStubFetcher{},
		Harness: runtime.NewHarness(registry, logger, runtime.DefaultValidationBudget),
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

// pushWithHash pushes a scene declaring lsml_bundle_hash and returns the
// decoded 200 body.
func pushWithHash(t *testing.T, mode config.LSDPMode, canvasHash string) map[string]any {
	t.Helper()
	srv, st := lsmlWireServer(t, mode)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "lsml-wire"); err != nil {
		t.Fatalf("create scene: %v", err)
	}
	body := `{"canvas_version":"v1","blue_blueprint_id":"bp-1","lsml_bundle_hash":"` + canvasHash + `"}`
	code, out := switchPost(t, srv.URL+"/api/v1/scenes/"+sceneID.String()+"/push", body)
	if code != 200 {
		t.Fatalf("push status = %d, want 200 (a hash mismatch NEVER fails the push): %v", code, out)
	}
	return out
}

func warningsOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	diags, ok := body["diagnostics"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no diagnostics object: %v", body)
	}
	warns, ok := diags["warnings"].([]any)
	if !ok {
		t.Fatalf("diagnostics.warnings is not an array: %v", diags)
	}
	return warns
}

// A drifted hash reaches the producer as a structured warning on the 200.
func TestPushWire_MismatchSurfacesWarning(t *testing.T) {
	// Deliberately not the hash of anything: whatever the stub compiles to,
	// this is not it.
	const bogus = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

	body := pushWithHash(t, config.LSDPModeDual, bogus)
	warns := warningsOf(t, body)
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one LSML_HASH_MISMATCH", warns)
	}
	w, ok := warns[0].(map[string]any)
	if !ok {
		t.Fatalf("warning is not an object: %v", warns[0])
	}
	if w["code"] != string(compiler.WarnLSMLHashMismatch) {
		t.Fatalf("code = %v, want %q", w["code"], compiler.WarnLSMLHashMismatch)
	}
	if w["severity"] != "warning" {
		t.Fatalf("severity = %v, want \"warning\"", w["severity"])
	}
	// The push still succeeded and kept the legacy mint — the warning is
	// information, not a refusal.
	if sv, _ := body["scene_version"].(string); sv == bogus {
		t.Fatal("scene_version must NOT be the supplied hash: identity did not collapse")
	}
}

// Bespoke mode is the default deployment and never even reaches the C4
// reconciliation. It must stay silent: warning here would fire on every push
// of every install that has not migrated, which is exactly the noise that
// makes a real drift unnoticeable.
func TestPushWire_BespokeModeEmitsNoWarning(t *testing.T) {
	const bogus = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

	body := pushWithHash(t, config.LSDPModeBespoke, bogus)
	if warns := warningsOf(t, body); len(warns) != 0 {
		t.Fatalf("bespoke mode must emit no warning, got %v", warns)
	}
}

// No hash supplied → nothing to contradict → no warning, in any mode.
func TestPushWire_AbsentHashEmitsNoWarning(t *testing.T) {
	srv, st := lsmlWireServer(t, config.LSDPModeDual)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "lsml-wire-absent"); err != nil {
		t.Fatalf("create scene: %v", err)
	}
	code, out := switchPost(t,
		srv.URL+"/api/v1/scenes/"+sceneID.String()+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"bp-1"}`)
	if code != 200 {
		t.Fatalf("push status = %d, want 200: %v", code, out)
	}
	if warns := warningsOf(t, out); len(warns) != 0 {
		t.Fatalf("absent hash must emit no warning, got %v", warns)
	}
}
