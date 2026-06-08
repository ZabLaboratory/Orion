//go:build e2e

// Package contract: API-level contract tests for POST /api/v1/scenes/{id}/push.
//
// These tests prove ADR-002 §6 at the HTTP boundary — not just the store
// layer. They require a live Postgres (ORION_E2E_DATABASE_URL) and exercise
// the full request/response cycle through httptest.Server.
//
// Resolution criteria covered (ADR-002 §6):
//   6.1 First push on an unseeded scene → 200, scene_version non-empty,
//       scenes row created (status=active, name=placeholder=scene_id,
//       latest_pushed_version set).
//   6.2 Re-push idempotent → 200, same or newer version, no row duplication.
//   6.3 Re-push preserves name/status (upsert DO UPDATE SET id=id).
//   6.4 Concurrent first-push race-safe (R2): N goroutines, all 200, one row.
//   6.5 Archived scene → 409 SCENE_ARCHIVED (guard preserved, no resurrection).
//   6.6 latest_pushed_version set + FK scene_definitions.scene_id satisfied.
//   6.8 scene_version deterministic across two pushes of same envelope.
//   auth gate: push without operator headers → 403.
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/store"
)

// operatorHeaders adds the ZabGate trust headers that requireOperator expects.
func operatorHeaders(r *http.Request) {
	r.Header.Set("X-Authenticated-User", "probe-test-operator")
	r.Header.Set("X-Authenticated-Role", "operator")
}

// doPush sends POST /api/v1/scenes/{id}/push with the default stub envelope.
// The returned *http.Response body MUST be read and closed by the caller.
func doPush(t *testing.T, ts interface{ URL string }, sceneID uuid.UUID) *http.Response {
	t.Helper()
	return doPushEnvelope(t, ts, sceneID, defaultEnvelope())
}

// doPushEnvelope sends POST /api/v1/scenes/{id}/push with the given envelope.
func doPushEnvelope(t *testing.T, ts interface{ URL string }, sceneID uuid.UUID, env interface{}) *http.Response {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost,
		fmt.Sprintf("%s/api/v1/scenes/%s/push", ts.URL, sceneID),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("build push request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	operatorHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("push request: %v", err)
	}
	return resp
}

// decodeBody decodes a push response body and closes it.
func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// §6.1 + §6.6 — First push creates row, sets pointer
// ─────────────────────────────────────────────────────────────────────────────

// TestE2E_PushAPI_FirstPush_CreatesRow (criterion 6.1 + 6.6):
// POST /api/v1/scenes/{id}/push on a scene that NEVER existed in Orion
// must return 200 with a non-empty scene_version, and the scenes row must
// be visible with status=active, name=placeholder (scene_id), and
// latest_pushed_version matching the returned scene_version.
func TestE2E_PushAPI_FirstPush_CreatesRow(t *testing.T) {
	st := requireDB(t)
	ts := newServer(t, st)
	defer ts.Close()

	sceneID := freshID()
	ctx := context.Background()

	// Pre-condition: the row does NOT exist.
	if _, err := st.GetScene(ctx, sceneID); err == nil {
		t.Fatal("scene unexpectedly pre-existed — test isolation broken")
	}

	resp := doPush(t, ts, sceneID)
	body := decodeBody(t, resp)

	// 200, not 404 (ADR-002 §6.1 — no NOT_FOUND on first push).
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("criterion 6.1: first push on unseeded scene returned %d (body=%v), want 200", resp.StatusCode, body)
	}

	// scene_version must be present and non-empty.
	sv, _ := body["scene_version"].(string)
	if sv == "" {
		t.Fatalf("criterion 6.1: scene_version missing or empty in push response: %v", body)
	}

	// The scenes row must now exist.
	scene, err := st.GetScene(ctx, sceneID)
	if err != nil {
		t.Fatalf("criterion 6.1: GetScene after first push: %v", err)
	}

	// status = active.
	if scene.Status != store.SceneActive {
		t.Fatalf("criterion 6.1: scene status = %q, want active", scene.Status)
	}

	// name = placeholder = scene_id (ADR-002 §3.2).
	if scene.Name != sceneID.String() {
		t.Fatalf("criterion 6.1: scene name = %q, want placeholder %q (scene_id)", scene.Name, sceneID.String())
	}

	// latest_pushed_version = scene_version returned (criterion 6.6).
	if scene.LatestPushedVersion == nil {
		t.Fatal("criterion 6.6: latest_pushed_version is NULL after first push — must be set")
	}
	if *scene.LatestPushedVersion != sv {
		t.Fatalf("criterion 6.6: latest_pushed_version = %q, want scene_version %q", *scene.LatestPushedVersion, sv)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// §6.2 — Re-push idempotent (no duplication)
// ─────────────────────────────────────────────────────────────────────────────

// TestE2E_PushAPI_RePush_Idempotent (criterion 6.2):
// A second push of the same scene must return 200 and must not duplicate the
// scenes row.
func TestE2E_PushAPI_RePush_Idempotent(t *testing.T) {
	st := requireDB(t)
	ts := newServer(t, st)
	defer ts.Close()

	sceneID := freshID()
	ctx := context.Background()

	// First push.
	resp1 := doPush(t, ts, sceneID)
	body1 := decodeBody(t, resp1)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first push: got %d, want 200 (body=%v)", resp1.StatusCode, body1)
	}
	sv1, _ := body1["scene_version"].(string)
	if sv1 == "" {
		t.Fatal("first push: scene_version empty")
	}

	// Second push (re-push).
	resp2 := doPush(t, ts, sceneID)
	body2 := decodeBody(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("criterion 6.2: re-push returned %d, want 200 (body=%v)", resp2.StatusCode, body2)
	}
	sv2, _ := body2["scene_version"].(string)
	if sv2 == "" {
		t.Fatalf("criterion 6.2: scene_version missing in re-push response: %v", body2)
	}

	// Exactly one scenes row must exist.
	scene, err := st.GetScene(ctx, sceneID)
	if err != nil {
		t.Fatalf("criterion 6.2: GetScene after re-push: %v", err)
	}
	if scene.ID != sceneID {
		t.Fatalf("criterion 6.2: scene id mismatch after re-push")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// §6.3 — Re-push preserves name / status
// ─────────────────────────────────────────────────────────────────────────────

// TestE2E_PushAPI_RePush_PreservesName (criterion 6.3):
// Re-pushing a scene whose name was set to a human name must NOT reset it.
func TestE2E_PushAPI_RePush_PreservesName(t *testing.T) {
	st := requireDB(t)
	ts := newServer(t, st)
	defer ts.Close()

	sceneID := freshID()
	ctx := context.Background()

	// First push — creates the row.
	resp1 := doPush(t, ts, sceneID)
	body1 := decodeBody(t, resp1)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first push: %d %v", resp1.StatusCode, body1)
	}

	// Simulate an operator renaming the scene.
	const humanName = "Grand Finals Scene"
	if _, err := st.Pool().Exec(ctx,
		`UPDATE scenes SET name = $2 WHERE id = $1`, sceneID, humanName,
	); err != nil {
		t.Fatalf("rename scene: %v", err)
	}

	// Re-push.
	resp2 := doPush(t, ts, sceneID)
	body2 := decodeBody(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("criterion 6.3: re-push returned %d (body=%v), want 200", resp2.StatusCode, body2)
	}

	// Name must be preserved (upsert DO UPDATE SET id=id never touches name).
	scene, err := st.GetScene(ctx, sceneID)
	if err != nil {
		t.Fatalf("criterion 6.3: GetScene: %v", err)
	}
	if scene.Name != humanName {
		t.Fatalf("criterion 6.3: name = %q after re-push, want %q — upsert must not overwrite", scene.Name, humanName)
	}
	if scene.Status != store.SceneActive {
		t.Fatalf("criterion 6.3: status = %q, want active preserved", scene.Status)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// §6.4 — Concurrent first-push race-safe (R2)
// ─────────────────────────────────────────────────────────────────────────────

// TestE2E_PushAPI_ConcurrentFirstPush_RaceSafe (criterion 6.4 / R2):
// N concurrent first-pushes of the SAME scene id must ALL return 200 and
// leave exactly one scenes row. No duplicate-key or FK violation must surface.
func TestE2E_PushAPI_ConcurrentFirstPush_RaceSafe(t *testing.T) {
	st := requireDB(t)
	ts := newServer(t, st)
	defer ts.Close()

	sceneID := freshID()
	ctx := context.Background()

	const racers = 6
	type result struct {
		status int
		body   map[string]any
	}
	results := make([]result, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp := doPush(t, ts, sceneID)
			body := decodeBody(t, resp)
			results[idx] = result{status: resp.StatusCode, body: body}
		}(i)
	}
	wg.Wait()

	// All goroutines must have returned 200.
	for i, r := range results {
		if r.status != http.StatusOK {
			t.Errorf("criterion 6.4: racer %d returned %d (body=%v), want 200", i, r.status, r.body)
		}
		sv, _ := r.body["scene_version"].(string)
		if sv == "" {
			t.Errorf("criterion 6.4: racer %d: scene_version missing", i)
		}
	}

	// Exactly one scenes row must exist.
	scene, err := st.GetScene(ctx, sceneID)
	if err != nil {
		t.Fatalf("criterion 6.4: GetScene after concurrent push: %v", err)
	}
	if scene.ID != sceneID {
		t.Fatalf("criterion 6.4: unexpected scene id after concurrent push")
	}
	if scene.LatestPushedVersion == nil {
		t.Fatal("criterion 6.4: latest_pushed_version NULL after concurrent pushes")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// §6.5 — Archived scene → 409 SCENE_ARCHIVED
// ─────────────────────────────────────────────────────────────────────────────

// TestE2E_PushAPI_Archived_Returns409 (criterion 6.5):
// Pushing to an archived scene must return 409 SCENE_ARCHIVED.
// The upsert must NOT resurrect the archived row.
func TestE2E_PushAPI_Archived_Returns409(t *testing.T) {
	st := requireDB(t)
	ts := newServer(t, st)
	defer ts.Close()

	sceneID := freshID()
	ctx := context.Background()

	if _, err := st.CreateScene(ctx, sceneID, "will-be-archived"); err != nil {
		t.Fatalf("CreateScene: %v", err)
	}
	if err := st.SetSceneStatus(ctx, sceneID, store.SceneArchived); err != nil {
		t.Fatalf("SetSceneStatus archived: %v", err)
	}

	resp := doPush(t, ts, sceneID)
	body := decodeBody(t, resp)

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("criterion 6.5: push on archived scene returned %d, want 409 (body=%v)", resp.StatusCode, body)
	}
	code, _ := body["code"].(string)
	if code != "SCENE_ARCHIVED" {
		t.Fatalf("criterion 6.5: error code = %q, want SCENE_ARCHIVED (body=%v)", code, body)
	}

	// The row must still be archived (upsert did not resurrect it).
	scene, err := st.GetScene(ctx, sceneID)
	if err != nil {
		t.Fatalf("criterion 6.5: GetScene: %v", err)
	}
	if scene.Status != store.SceneArchived {
		t.Fatalf("criterion 6.5: scene status = %q, want archived after rejection", scene.Status)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// §6.6 (FK) — InsertDefinition succeeds on first push
// ─────────────────────────────────────────────────────────────────────────────

// TestE2E_PushAPI_FK_Satisfied (criterion 6.6 / FK):
// The FK scene_definitions.scene_id → scenes(id) must be satisfied on a
// first push. Assert that a scene_definitions row exists after first push.
func TestE2E_PushAPI_FK_Satisfied(t *testing.T) {
	st := requireDB(t)
	ts := newServer(t, st)
	defer ts.Close()

	sceneID := freshID()
	ctx := context.Background()

	resp := doPush(t, ts, sceneID)
	body := decodeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first push: %d %v", resp.StatusCode, body)
	}

	var count int
	if err := st.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM scene_definitions WHERE scene_id = $1`, sceneID,
	).Scan(&count); err != nil {
		t.Fatalf("criterion 6.6/FK: query definitions: %v", err)
	}
	if count == 0 {
		t.Fatal("criterion 6.6/FK: no scene_definitions row — FK was not satisfied or InsertDefinition was skipped")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// §6.8 — scene_version determinism
// ─────────────────────────────────────────────────────────────────────────────

// TestE2E_PushAPI_SceneVersion_Deterministic (criterion 6.8):
// Re-pushing the same scene with the same envelope must produce the same
// scene_version. The lifecycle (auto-create) must not enter the hash.
func TestE2E_PushAPI_SceneVersion_Deterministic(t *testing.T) {
	st := requireDB(t)
	ts := newServer(t, st)
	defer ts.Close()

	sceneID := freshID()

	// First push (auto-creates row).
	resp1 := doPush(t, ts, sceneID)
	body1 := decodeBody(t, resp1)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first push: %d %v", resp1.StatusCode, body1)
	}
	sv1, _ := body1["scene_version"].(string)
	if sv1 == "" {
		t.Fatal("first push: scene_version empty")
	}

	// Re-push with identical envelope — version must be identical.
	resp2 := doPush(t, ts, sceneID)
	body2 := decodeBody(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("re-push: %d %v", resp2.StatusCode, body2)
	}
	sv2, _ := body2["scene_version"].(string)
	if sv2 == "" {
		t.Fatal("re-push: scene_version empty")
	}
	if sv1 != sv2 {
		t.Fatalf("criterion 6.8: scene_version not deterministic: push1=%q push2=%q", sv1, sv2)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth gate
// ─────────────────────────────────────────────────────────────────────────────

// TestE2E_PushAPI_AuthGate_Rejects:
// A push without the operator trust headers must return 403. This confirms
// the requireOperator guard is wired on the push route.
func TestE2E_PushAPI_AuthGate_Rejects(t *testing.T) {
	st := requireDB(t)
	ts := newServer(t, st)
	defer ts.Close()

	sceneID := freshID()
	body, err := json.Marshal(defaultEnvelope())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost,
		fmt.Sprintf("%s/api/v1/scenes/%s/push", ts.URL, sceneID),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Intentionally NO auth headers.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("auth gate: got %d, want 403", resp.StatusCode)
	}
}
