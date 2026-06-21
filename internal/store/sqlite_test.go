package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// openTestSQLite opens a fresh, migrated SQLite store in a temp file (one per
// test). The file path keeps WAL behaviour realistic; :memory: would share no
// state across the pool's connections.
func openTestSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orion.db")
	st, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestSQLiteStore_SatisfiesStore is the RC-3 type guard for the second backend.
func TestSQLiteStore_SatisfiesStore(_ *testing.T) {
	var _ Store = (*SQLiteStore)(nil)
}

// TestSQLiteStore_MigrateIdempotent: a second open over the same file re-runs
// migrations as a no-op (tracked in schema_migrations).
func TestSQLiteStore_MigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orion.db")
	st1, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	st1.Close()
	st2, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("second open (re-migrate) must be a no-op: %v", err)
	}
	st2.Close()
}

// TestSQLiteStore_BootExecRoundTrip exercises every operation the boot/exec
// path uses against the SQLite backend: scene create/upsert, push (definition
// + pushed version + pointer in one tx), cold-start enumeration, active-scene
// pointer, stream rules, and validation gate. This is the behaviour the
// parity suite (e2e) re-asserts against PGStore.
func TestSQLiteStore_BootExecRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTestSQLite(t)

	sceneID := uuid.New()
	defID := uuid.New()
	const ver = "abc123def456"

	if _, err := st.UpsertScene(ctx, sceneID, "placeholder"); err != nil {
		t.Fatalf("UpsertScene: %v", err)
	}
	// Idempotent re-upsert keeps the row.
	if _, err := st.UpsertScene(ctx, sceneID, "ignored"); err != nil {
		t.Fatalf("UpsertScene (re): %v", err)
	}

	// Push: definition + pushed version + pointer in ONE tx.
	err := st.Tx(ctx, func(tx Tx) error {
		next, err := st.NextDefinitionVersionTx(ctx, tx, sceneID)
		if err != nil {
			return err
		}
		if next != 1 {
			t.Fatalf("first definition version = %d, want 1", next)
		}
		if err := st.InsertDefinitionTx(ctx, tx, SceneDefinition{
			ID: defID, SceneID: sceneID, DefinitionVersion: next,
			CanvasVersion: "canvas-v1", BlueBlueprintID: "bp-1",
			ComponentsJSON: json.RawMessage(`[{"x":1}]`),
		}); err != nil {
			return err
		}
		if err := st.InsertPushedVersion(ctx, tx, ScenePushedVersion{
			SceneID: sceneID, SceneVersion: ver, DefinitionID: defID,
			GraphJSON:  json.RawMessage(`{"nodes":[]}`),
			BundleJSON: json.RawMessage(`{"root":{}}`),
		}); err != nil {
			return err
		}
		v := ver
		return st.SetLatestPushedVersion(ctx, tx, sceneID, &v)
	})
	if err != nil {
		t.Fatalf("push tx: %v", err)
	}

	// Cold-start enumeration sees the active+pushed scene.
	scenes, err := st.ListActiveScenesWithPush(ctx)
	if err != nil {
		t.Fatalf("ListActiveScenesWithPush: %v", err)
	}
	if len(scenes) != 1 || scenes[0].ID != sceneID {
		t.Fatalf("cold-start roster = %+v", scenes)
	}
	if scenes[0].LatestPushedVersion == nil || *scenes[0].LatestPushedVersion != ver {
		t.Fatalf("latest pushed version not set: %+v", scenes[0].LatestPushedVersion)
	}

	// GetLatestPushedVersion resolves the pointer + artefacts.
	pv, err := st.GetLatestPushedVersion(ctx, sceneID)
	if err != nil {
		t.Fatalf("GetLatestPushedVersion: %v", err)
	}
	if pv.SceneVersion != ver || string(pv.GraphJSON) != `{"nodes":[]}` {
		t.Fatalf("pushed version round-trip mismatch: %+v", pv)
	}

	// Active-scene pointer round-trips (nil → set → read).
	if got, err := st.GetActiveSceneID(ctx); err != nil || got != nil {
		t.Fatalf("active pointer should start nil: %v %v", got, err)
	}
	if err := st.SetActiveSceneID(ctx, &sceneID); err != nil {
		t.Fatalf("SetActiveSceneID: %v", err)
	}
	got, err := st.GetActiveSceneID(ctx)
	if err != nil || got == nil || *got != sceneID {
		t.Fatalf("active pointer = %v %v", got, err)
	}

	// Stream rules.
	if err := st.AddStreamRule(ctx, sceneID); err != nil {
		t.Fatalf("AddStreamRule: %v", err)
	}
	if isRule, err := st.IsStreamRule(ctx, sceneID); err != nil || !isRule {
		t.Fatalf("IsStreamRule = %v %v", isRule, err)
	}
	rules, err := st.ListStreamRules(ctx)
	if err != nil || len(rules) != 1 || rules[0] != sceneID {
		t.Fatalf("ListStreamRules = %+v %v", rules, err)
	}

	// Validation gate: no record ⇒ not eligible; validated record ⇒ eligible.
	if ok, err := st.IsVersionValidated(ctx, sceneID, ver, "h1"); err != nil || ok {
		t.Fatalf("unvalidated must be false: %v %v", ok, err)
	}
	if err := st.UpsertValidation(ctx, SceneValidation{
		SceneID: sceneID, SceneVersion: ver, HarnessVersion: "h1",
		Status: ValidationValidated, Report: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("UpsertValidation: %v", err)
	}
	if ok, err := st.IsVersionValidated(ctx, sceneID, ver, "h1"); err != nil || !ok {
		t.Fatalf("validated must be true: %v %v", ok, err)
	}
}

// TestSQLiteStore_NotFound: a missing row maps to ErrNotFound, backend-agnostic.
func TestSQLiteStore_NotFound(t *testing.T) {
	ctx := context.Background()
	st := openTestSQLite(t)
	if _, err := st.GetScene(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetScene miss = %v, want ErrNotFound", err)
	}
	if _, err := st.GetPushedVersion(ctx, uuid.New(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPushedVersion miss = %v, want ErrNotFound", err)
	}
}

// TestSQLiteStore_ForeignKeysEnforced confirms PRAGMA foreign_keys is on:
// a definition referencing a non-existent scene must be rejected, matching
// the pg FK behaviour the schema mirrors.
func TestSQLiteStore_ForeignKeysEnforced(t *testing.T) {
	ctx := context.Background()
	st := openTestSQLite(t)
	err := st.InsertDefinition(ctx, SceneDefinition{
		ID: uuid.New(), SceneID: uuid.New(), DefinitionVersion: 1,
		CanvasVersion: "v", BlueBlueprintID: "bp",
	})
	if err == nil {
		t.Fatal("FK violation should reject an orphan definition")
	}
}

// TestSQLiteStore_ArchivePurge mirrors the archive path: purge pushed versions
// + null the pointer in one tx, then flip status. The validation record
// cascades away with its pushed version (FK ON DELETE CASCADE).
func TestSQLiteStore_ArchivePurge(t *testing.T) {
	ctx := context.Background()
	st := openTestSQLite(t)
	sceneID, defID := uuid.New(), uuid.New()
	const ver = "v-purge"

	if _, err := st.UpsertScene(ctx, sceneID, "s"); err != nil {
		t.Fatal(err)
	}
	if err := st.Tx(ctx, func(tx Tx) error {
		n, err := st.NextDefinitionVersionTx(ctx, tx, sceneID)
		if err != nil {
			return err
		}
		if err := st.InsertDefinitionTx(ctx, tx, SceneDefinition{
			ID: defID, SceneID: sceneID, DefinitionVersion: n,
			CanvasVersion: "c", BlueBlueprintID: "b",
		}); err != nil {
			return err
		}
		if err := st.InsertPushedVersion(ctx, tx, ScenePushedVersion{
			SceneID: sceneID, SceneVersion: ver, DefinitionID: defID,
			GraphJSON: json.RawMessage(`{}`), BundleJSON: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		v := ver
		return st.SetLatestPushedVersion(ctx, tx, sceneID, &v)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.UpsertValidation(ctx, SceneValidation{
		SceneID: sceneID, SceneVersion: ver, HarnessVersion: "h1",
		Status: ValidationValidated, Report: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("validation: %v", err)
	}

	if err := st.Tx(ctx, func(tx Tx) error {
		if _, err := st.PurgePushedVersions(ctx, tx, sceneID); err != nil {
			return err
		}
		var nullPtr *string
		return st.SetLatestPushedVersion(ctx, tx, sceneID, nullPtr)
	}); err != nil {
		t.Fatalf("purge tx: %v", err)
	}
	if err := st.SetSceneStatus(ctx, sceneID, SceneArchived); err != nil {
		t.Fatalf("SetSceneStatus: %v", err)
	}

	if _, err := st.GetLatestPushedVersion(ctx, sceneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged version still resolvable: %v", err)
	}
	// Validation record cascaded away with the pushed version.
	if _, err := st.GetValidation(ctx, sceneID, ver, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("validation should cascade on purge: %v", err)
	}
}
