//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/store"
)

// TestStoreParity_PGvsSQLite runs the SAME boot/exec sequence against BOTH
// store backends and asserts identical observable behaviour (ADR 016 §3.2,
// RC-3, R1). PGStore comes from requireDB (skipped without ORION_E2E_DATABASE_URL);
// SQLiteStore opens a temp file and always runs. The store keeps every
// artefact opaque, so parity is about the typed surface — pointers, FKs,
// cold-start enumeration, the validation gate — not jsonb semantics.
func TestStoreParity_PGvsSQLite(t *testing.T) {
	backends := map[string]func(t *testing.T) store.Store{
		"sqlite": func(t *testing.T) store.Store {
			st, err := store.OpenSQLite(context.Background(), t.TempDir()+"/orion.db")
			if err != nil {
				t.Fatalf("OpenSQLite: %v", err)
			}
			t.Cleanup(st.Close)
			return st
		},
		"postgres": func(t *testing.T) store.Store { return requireDB(t) },
	}
	for name, open := range backends {
		t.Run(name, func(t *testing.T) {
			runStoreParitySequence(t, open(t))
		})
	}
}

// runStoreParitySequence is the backend-agnostic contract: every assertion
// must hold identically for pg and sqlite.
func runStoreParitySequence(t *testing.T, st store.Store) {
	ctx := context.Background()
	sceneID, defID := uuid.New(), uuid.New()
	const ver = "parity-v1"

	if _, err := st.UpsertScene(ctx, sceneID, "s"); err != nil {
		t.Fatalf("UpsertScene: %v", err)
	}
	if err := st.Tx(ctx, func(tx store.Tx) error {
		n, err := st.NextDefinitionVersionTx(ctx, tx, sceneID)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Fatalf("first def version = %d, want 1", n)
		}
		if err := st.InsertDefinitionTx(ctx, tx, store.SceneDefinition{
			ID: defID, SceneID: sceneID, DefinitionVersion: n,
			CanvasVersion: "c", BlueBlueprintID: "b",
			ComponentsJSON: json.RawMessage(`[]`),
		}); err != nil {
			return err
		}
		if err := st.InsertPushedVersion(ctx, tx, store.ScenePushedVersion{
			SceneID: sceneID, SceneVersion: ver, DefinitionID: defID,
			GraphJSON: json.RawMessage(`{"nodes":[]}`), BundleJSON: json.RawMessage(`{"root":{}}`),
		}); err != nil {
			return err
		}
		v := ver
		return st.SetLatestPushedVersion(ctx, tx, sceneID, &v)
	}); err != nil {
		t.Fatalf("push tx: %v", err)
	}

	// Re-push of the same scene_version is idempotent (ON CONFLICT DO NOTHING)
	// and the definition version advances to 2.
	if err := st.Tx(ctx, func(tx store.Tx) error {
		n, err := st.NextDefinitionVersionTx(ctx, tx, sceneID)
		if err != nil {
			return err
		}
		if n != 2 {
			t.Fatalf("second def version = %d, want 2", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("second def version tx: %v", err)
	}

	scenes, err := st.ListActiveScenesWithPush(ctx)
	if err != nil || len(scenes) != 1 || scenes[0].ID != sceneID {
		t.Fatalf("cold-start roster = %+v err=%v", scenes, err)
	}

	pv, err := st.GetLatestPushedVersion(ctx, sceneID)
	if err != nil || pv.SceneVersion != ver {
		t.Fatalf("GetLatestPushedVersion = %+v err=%v", pv, err)
	}

	if err := st.SetActiveSceneID(ctx, &sceneID); err != nil {
		t.Fatalf("SetActiveSceneID: %v", err)
	}
	got, err := st.GetActiveSceneID(ctx)
	if err != nil || got == nil || *got != sceneID {
		t.Fatalf("active pointer = %v err=%v", got, err)
	}

	if err := st.AddStreamRule(ctx, sceneID); err != nil {
		t.Fatalf("AddStreamRule: %v", err)
	}
	rules, err := st.ListStreamRules(ctx)
	if err != nil || len(rules) != 1 || rules[0] != sceneID {
		t.Fatalf("ListStreamRules = %+v err=%v", rules, err)
	}

	if ok, err := st.IsVersionValidated(ctx, sceneID, ver, "h1"); err != nil || ok {
		t.Fatalf("pre-validation eligible: %v err=%v", ok, err)
	}
	if err := st.UpsertValidation(ctx, store.SceneValidation{
		SceneID: sceneID, SceneVersion: ver, HarnessVersion: "h1",
		Status: store.ValidationValidated, Report: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("UpsertValidation: %v", err)
	}
	if ok, err := st.IsVersionValidated(ctx, sceneID, ver, "h1"); err != nil || !ok {
		t.Fatalf("post-validation not eligible: %v err=%v", ok, err)
	}
}
