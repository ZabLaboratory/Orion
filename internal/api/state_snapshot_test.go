package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Preview→air state hand-off EXPORT seam tests (ADR Prism 005 Amendment 2
// §A2.2.d, Orion #256). The IMPORT side (b) — POST /show/active-scene
// { state_snapshot } — is RETIRED (#15, #331, ADR-BLUE-012 invariant #8);
// its tests (VETO 1/2/3/4/5/6/8/9, validateSnapshotForSeed, isReservedPath,
// snapshotRoleRefused) retired with it.

const snapSceneID = "22222222-2222-2222-2222-222222222222"
const snapVersion = "sha256:v1"

func newSnapFixture(t *testing.T, profile config.Profile) *http.ServeMux {
	t.Helper()
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	show.SetExecMetrics(m)
	t.Cleanup(show.Stop)

	graph := &compiler.Graph{
		SceneID: snapSceneID, SceneVersion: snapVersion,
		Defaults: map[string]json.RawMessage{
			"score.blue": json.RawMessage(`0`),
			"team.name":  json.RawMessage(`""`),
		},
	}
	show.LoadExec(snapSceneID, graph, &compiler.RenderBundle{SceneVersion: snapVersion})

	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger:  testLogger(),
		Metrics: m,
		Show:    show,
		Config:  config.Config{Profile: profile},
	})
	return mux
}

func snapReq(t *testing.T, mux *http.ServeMux, method, path, role string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, &buf)
	if role != "" {
		r.Header.Set("X-Authenticated-User", "op")
		r.Header.Set("X-Authenticated-Role", role)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestExport_Seam_PreviewOnly_AntenneIs404(t *testing.T) {
	mux := newSnapFixture(t, config.ProfileAntenne)
	w := snapReq(t, mux, "GET", "/api/v1/scenes/"+snapSceneID+"/state-snapshot", "operator", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("VETO #11: antenne export must 404, got %d", w.Code)
	}
}

func TestExport_Seam_Sidecar_ReturnsSnapshot(t *testing.T) {
	mux := newSnapFixture(t, config.ProfileEmbeddedLocal)
	w := snapReq(t, mux, "GET", "/api/v1/scenes/"+snapSceneID+"/state-snapshot", "operator", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sidecar export: got %d, want 200", w.Code)
	}
	var got stateSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != snapVersion {
		t.Fatalf("export version = %q, want %q", got.Version, snapVersion)
	}
	if _, ok := got.State["score.blue"]; !ok {
		t.Fatal("export missing seeded default leaf")
	}
}

func TestExport_Seam_RequiresOperator(t *testing.T) {
	mux := newSnapFixture(t, config.ProfileEmbeddedLocal)
	w := snapReq(t, mux, "GET", "/api/v1/scenes/"+snapSceneID+"/state-snapshot", "viewer", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer export: got %d, want 403", w.Code)
	}
}

// TestExport_Seam_Session_ReadsIsolatedSession proves the hand-off export
// reads the ISOLATED preview test-session ("?session="), not the global
// show: the session clone holds a value (42) the show's active scene never
// saw (its default is 0). This is what makes the preview→air hand-off carry
// the preview prep now that the preview lives in a session, not the show.
func TestExport_Seam_Session_ReadsIsolatedSession(t *testing.T) {
	m := obs.NewMetrics()
	registry := runtime.NewComputeRegistry()
	logger := testLogger()
	show := runtime.NewShow(registry, logger)
	t.Cleanup(show.Stop)

	// Global show: score.blue default 0 (the value the antenne would see).
	showGraph := &compiler.Graph{
		SceneID: snapSceneID, SceneVersion: snapVersion,
		Defaults: map[string]json.RawMessage{"score.blue": json.RawMessage(`0`)},
	}
	show.LoadExec(snapSceneID, showGraph, &compiler.RenderBundle{SceneVersion: snapVersion})

	// Isolated preview session on the SAME scene id, with a passthrough so an
	// input materialises on its clone — never on the show.
	mgr := runtime.NewTestSessionManager(registry, logger, 5*time.Minute)
	t.Cleanup(mgr.Close)
	sessGraph := &compiler.Graph{
		SceneID: snapSceneID, SceneVersion: snapVersion,
		Nodes: []compiler.GraphNode{
			{ID: "in.b", Kind: "input"},
			{ID: "out.b", Kind: "output", Path: "score.blue", Compute: "core.passthrough", Upstream: []string{"in.b"}},
		},
		Defaults: map[string]json.RawMessage{"score.blue": json.RawMessage(`0`)},
	}
	sessionID, scene := mgr.Open(context.Background(), snapSceneID, sessGraph, &compiler.RenderBundle{SceneVersion: snapVersion})
	if !scene.Input(runtime.InputMsg{Path: "score.blue", Value: json.RawMessage(`42`)}) {
		t.Fatal("session inbox full")
	}
	// Let the clone's loop apply the input before exporting.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, st, _ := mgr.SnapshotState(sessionID); string(st["score.blue"]) == "42" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger: logger, Metrics: m, Show: show, Test: mgr,
		Config: config.Config{Profile: config.ProfileEmbeddedLocal},
	})

	// With ?session= → the session's 42.
	w := snapReq(t, mux, "GET", "/api/v1/scenes/"+snapSceneID+"/state-snapshot?session="+sessionID, "operator", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("session export: got %d, want 200", w.Code)
	}
	var got stateSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if string(got.State["score.blue"]) != "42" {
		t.Fatalf("session export score.blue = %q, want 42 (read the show, not the session?)", got.State["score.blue"])
	}

	// Without ?session= → the global show's 0 (unchanged path).
	w2 := snapReq(t, mux, "GET", "/api/v1/scenes/"+snapSceneID+"/state-snapshot", "operator", nil)
	var showGot stateSnapshot
	if err := json.Unmarshal(w2.Body.Bytes(), &showGot); err != nil {
		t.Fatal(err)
	}
	if string(showGot.State["score.blue"]) != "0" {
		t.Fatalf("show export score.blue = %q, want 0", showGot.State["score.blue"])
	}

	// A lapsed/unknown session → 410.
	w3 := snapReq(t, mux, "GET", "/api/v1/scenes/"+snapSceneID+"/state-snapshot?session=nope", "operator", nil)
	if w3.Code != http.StatusGone {
		t.Fatalf("unknown session export: got %d, want 410", w3.Code)
	}
}
