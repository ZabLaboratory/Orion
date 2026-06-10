//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/api"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// HTTP-level coverage of the validation gate's ENFORCEMENT (ADR 003
// §3.2.2, criteria 12 + 15): the real push / active-scene / validate
// handlers wired through RegisterPublic, against a live Postgres and ONE
// shared runtime Show (so the antenna assertion is faithful).
//
//   - criterion 12: a never-validated version cannot be activated
//     (SCENE_NOT_VALIDATED); after the campaign validates it, activation
//     succeeds.
//   - criterion 15 (B3): with a scene ACTIVE on air, a re-push (a new hash
//     with no validation record) is PERSISTED (200) but the antenna does
//     NOT move — the response surfaces SCENE_NOT_VALIDATED and air_version
//     stays the previously-validated version.

func testGateLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func gateTestServer(t *testing.T, st *store.Store, fetcher compiler.Fetcher) (*httptest.Server, *runtime.Show) {
	t.Helper()
	logger := testGateLogger()
	metrics := obs.NewMetrics()
	registry := runtime.NewComputeRegistry()
	show := runtime.NewShow(registry, logger)
	t.Cleanup(show.Stop)
	testMgr := runtime.NewTestSessionManager(registry, logger, time.Minute)
	t.Cleanup(testMgr.Close)
	harness := runtime.NewHarness(registry, logger, runtime.DefaultValidationBudget)

	mux := http.NewServeMux()
	api.RegisterPublic(mux, api.PublicDeps{
		Logger:  logger,
		Metrics: metrics,
		Config:  config.Config{PushTimeout: 10 * time.Second, ValidationTimeout: 30 * time.Second},
		Show:    show,
		Test:    testMgr,
		Store:   st,
		Fetcher: fetcher,
		Harness: harness,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, show
}

func operatorPost(t *testing.T, url, body string) (int, map[string]any) {
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

func operatorGet(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// twoBlueprintFetcher serves two blueprints (bp-1, bp-2) writing distinct
// leaves, so pushing each mints a DISTINCT scene_version — one fetcher,
// one Show, two versions.
func twoBlueprintFetcher() *stubFetcher {
	return &stubFetcher{
		layouts: map[string]*compiler.CanvasLayout{
			"v1": {Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		},
		blueprints: map[string]*compiler.BlueprintGraph{
			"bp-1": {ID: "bp-1", Nodes: []compiler.BlueprintNode{
				{ID: "out.x", Compute: "core.input",
					Config: map[string]json.RawMessage{"name": json.RawMessage(`"score"`)}},
			}},
			"bp-2": {ID: "bp-2", Nodes: []compiler.BlueprintNode{
				{ID: "out.y", Compute: "core.input",
					Config: map[string]json.RawMessage{"name": json.RawMessage(`"score_v2"`)}},
			}},
		},
		manifest: compiler.ComputeManifest{
			"core.input": {IsPure: true, IsBounded: true, Version: "1"},
		},
	}
}

func waitValidated(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, body := operatorGet(t, base+"/validation")
		if code == 200 && body["status"] == "validated" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("validation did not reach validated for %s", base)
}

// TestE2E_Gate_ActivateRequiresValidation (criterion 12): a pushed but
// never-validated scene cannot be activated; after validation it can.
func TestE2E_Gate_ActivateRequiresValidation(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "gate"); err != nil {
		t.Fatal(err)
	}
	srv, _ := gateTestServer(t, st, twoBlueprintFetcher())
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	if code, _ := operatorPost(t, base+"/push", `{"canvas_version":"v1","blue_blueprint_id":"bp-1"}`); code != 200 {
		t.Fatalf("push status = %d", code)
	}

	// Activate before validation → refused.
	code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene", `{"scene_id":"`+sceneID.String()+`"}`)
	if code != http.StatusConflict || body["code"] != "SCENE_NOT_VALIDATED" {
		t.Fatalf("activate-before-validate = %d %v, want 409 SCENE_NOT_VALIDATED", code, body)
	}

	if code, _ := operatorPost(t, base+"/validate", `{}`); code != http.StatusAccepted {
		t.Fatalf("validate status = %d", code)
	}
	waitValidated(t, base)

	if code, _ := operatorPost(t, srv.URL+"/api/v1/show/active-scene", `{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate-after-validate status = %d", code)
	}
}

// TestE2E_Gate_PushSwapKeepsAntenna (criterion 15, B3): with a scene active
// on air (v1, validated), a re-push of a DIFFERENT content (v2, no record)
// is persisted but the antenna does not move — SCENE_NOT_VALIDATED
// surfaced, air_version stays v1.
func TestE2E_Gate_PushSwapKeepsAntenna(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "gate2"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, twoBlueprintFetcher())
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	// Push v1 (bp-1), validate, activate.
	_, pushBody := operatorPost(t, base+"/push", `{"canvas_version":"v1","blue_blueprint_id":"bp-1"}`)
	v1, _ := pushBody["scene_version"].(string)
	operatorPost(t, base+"/validate", `{}`)
	waitValidated(t, base)
	if code, _ := operatorPost(t, srv.URL+"/api/v1/show/active-scene", `{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate status = %d", code)
	}
	if a := show.Active(); a == nil || a.Graph().SceneVersion != v1 {
		t.Fatalf("antenna not on v1 after activate")
	}

	// Re-push v2 (bp-2 → different hash) — never validated.
	code, body := operatorPost(t, base+"/push", `{"canvas_version":"v1","blue_blueprint_id":"bp-2"}`)
	if code != 200 {
		t.Fatalf("re-push status = %d %v", code, body)
	}
	v2, _ := body["scene_version"].(string)
	if v2 == v1 || v2 == "" {
		t.Fatalf("re-push minted hash %q (v1=%q); test needs distinct versions", v2, v1)
	}
	if body["code"] != "SCENE_NOT_VALIDATED" {
		t.Fatalf("re-push of active scene did not surface SCENE_NOT_VALIDATED: %v", body)
	}
	if av, _ := body["air_version"].(string); av != v1 {
		t.Fatalf("air_version = %q, want v1 %q", av, v1)
	}

	// The antenna must NOT have moved: still serving v1.
	if a := show.Active(); a == nil || a.Graph().SceneVersion != v1 {
		var got string
		if a := show.Active(); a != nil {
			got = a.Graph().SceneVersion
		}
		t.Fatalf("antenna moved to %q, want it to stay on v1 %q", got, v1)
	}
}
