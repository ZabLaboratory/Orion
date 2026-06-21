//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// E2E coverage of the scene-validation gate's STORE layer (ADR 003 §3.2.2,
// issue #87): the record that makes a version air-eligible, invalidation
// by construction (criterion 13), and the archive-purge cascade
// (criterion 16). The HTTP enforcement is thin glue over IsVersionValidated;
// these prove the persistence invariants the glue rests on against real PG.

// seedPushedVersion creates a scene + definition + one pushed version with
// the given scene_version hash. Returns nothing — the caller knows the ids.
func seedPushedVersion(t *testing.T, st store.Store, sceneID uuid.UUID, version string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.UpsertScene(ctx, sceneID, sceneID.String()); err != nil {
		t.Fatalf("upsert scene: %v", err)
	}
	// Bump the definition version per seed so two versions on the SAME
	// scene (invalidation / re-push tests) don't collide on
	// UNIQUE(scene_id, definition_version).
	maxVer, err := st.MaxDefinitionVersion(ctx, sceneID)
	if err != nil {
		t.Fatalf("max definition version: %v", err)
	}
	defID := uuid.New()
	if err := st.InsertDefinition(ctx, store.SceneDefinition{
		ID: defID, SceneID: sceneID, DefinitionVersion: maxVer + 1,
		CanvasVersion: "v1", BlueBlueprintID: "bp-1",
		ComponentsJSON: json.RawMessage(`[]`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert definition: %v", err)
	}
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		pv := store.ScenePushedVersion{
			SceneID: sceneID, SceneVersion: version, DefinitionID: defID,
			GraphJSON: json.RawMessage(`{}`), BundleJSON: json.RawMessage(`{}`),
			CreatedAt: time.Now(),
		}
		if err := st.InsertPushedVersion(ctx, tx, pv); err != nil {
			return err
		}
		return st.SetLatestPushedVersion(ctx, tx, sceneID, &version)
	}); err != nil {
		t.Fatalf("seed pushed version: %v", err)
	}
}

// TestE2E_Validation_NoRecordIsNotEligible: a freshly pushed version has no
// validation record → IsVersionValidated == false (the gate refuses).
func TestE2E_Validation_NoRecordIsNotEligible(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	seedPushedVersion(t, st, sceneID, "sha256:v1")

	ok, err := st.IsVersionValidated(context.Background(), sceneID, "sha256:v1", runtime.HarnessVersion)
	if err != nil {
		t.Fatalf("IsVersionValidated: %v", err)
	}
	if ok {
		t.Fatalf("a version with no record is air-eligible — gate is open")
	}
}

// TestE2E_Validation_ValidatedRecordIsEligible: a `validated` record for
// the current harness_version makes the version air-eligible; a `failed`
// record does NOT.
func TestE2E_Validation_ValidatedRecordIsEligible(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	sceneID := uuid.New()
	seedPushedVersion(t, st, sceneID, "sha256:v1")

	if err := st.UpsertValidation(ctx, store.SceneValidation{
		SceneID: sceneID, SceneVersion: "sha256:v1",
		HarnessVersion: runtime.HarnessVersion, Status: "validated",
		Report: json.RawMessage(`{"status":"validated"}`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert validated: %v", err)
	}
	ok, err := st.IsVersionValidated(ctx, sceneID, "sha256:v1", runtime.HarnessVersion)
	if err != nil || !ok {
		t.Fatalf("validated record not eligible: ok=%v err=%v", ok, err)
	}

	// A failed record overwrites and is NOT eligible.
	if err := st.UpsertValidation(ctx, store.SceneValidation{
		SceneID: sceneID, SceneVersion: "sha256:v1",
		HarnessVersion: runtime.HarnessVersion, Status: "failed",
		Report: json.RawMessage(`{"status":"failed"}`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	ok, err = st.IsVersionValidated(ctx, sceneID, "sha256:v1", runtime.HarnessVersion)
	if err != nil || ok {
		t.Fatalf("failed record is eligible: ok=%v err=%v", ok, err)
	}
}

// TestE2E_Validation_InvalidationByHash (criterion 13): a validated v1 plus
// a never-validated v2 — only v1 is eligible. A new version (a different
// hash) has no record and is refused until re-validated.
func TestE2E_Validation_InvalidationByHash(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	sceneID := uuid.New()
	seedPushedVersion(t, st, sceneID, "sha256:v1")

	if err := st.UpsertValidation(ctx, store.SceneValidation{
		SceneID: sceneID, SceneVersion: "sha256:v1",
		HarnessVersion: runtime.HarnessVersion, Status: "validated",
		Report: json.RawMessage(`{}`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// A change mints v2 (new hash). It has no record.
	seedPushedVersion(t, st, sceneID, "sha256:v2")

	if ok, _ := st.IsVersionValidated(ctx, sceneID, "sha256:v1", runtime.HarnessVersion); !ok {
		t.Fatalf("v1 should stay validated")
	}
	if ok, _ := st.IsVersionValidated(ctx, sceneID, "sha256:v2", runtime.HarnessVersion); ok {
		t.Fatalf("v2 (new hash) is eligible without a record — invalidation broken")
	}
}

// TestE2E_Validation_HarnessVersionGate: a record validated under an OLD
// harness_version does not satisfy the gate at the CURRENT one (fleet-wide
// re-validation lever, §3.2.2).
func TestE2E_Validation_HarnessVersionGate(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	sceneID := uuid.New()
	seedPushedVersion(t, st, sceneID, "sha256:v1")

	if err := st.UpsertValidation(ctx, store.SceneValidation{
		SceneID: sceneID, SceneVersion: "sha256:v1",
		HarnessVersion: "old-harness", Status: "validated",
		Report: json.RawMessage(`{}`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if ok, _ := st.IsVersionValidated(ctx, sceneID, "sha256:v1", runtime.HarnessVersion); ok {
		t.Fatalf("an old-harness record satisfies the current gate — re-validation lever broken")
	}
}

// TestE2E_Validation_ArchivePurgeCascade (criterion 16): purging a
// version's artefacts deletes its validation records coherently. A re-push
// of byte-identical content re-mints the same hash but resurrects NO
// record — it must re-validate.
func TestE2E_Validation_ArchivePurgeCascade(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	sceneID := uuid.New()
	seedPushedVersion(t, st, sceneID, "sha256:v1")

	if err := st.UpsertValidation(ctx, store.SceneValidation{
		SceneID: sceneID, SceneVersion: "sha256:v1",
		HarnessVersion: runtime.HarnessVersion, Status: "validated",
		Report: json.RawMessage(`{}`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if ok, _ := st.IsVersionValidated(ctx, sceneID, "sha256:v1", runtime.HarnessVersion); !ok {
		t.Fatalf("precondition: v1 should be validated")
	}

	// Archive purge: delete the pushed versions. The FK ON DELETE CASCADE
	// must take the validation record with it.
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := st.PurgePushedVersions(ctx, tx, sceneID); err != nil {
			return err
		}
		var nullPtr *string
		return st.SetLatestPushedVersion(ctx, tx, sceneID, nullPtr)
	}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// Re-push byte-identical content → same hash, but no resurrected record.
	seedPushedVersion(t, st, sceneID, "sha256:v1")
	if ok, _ := st.IsVersionValidated(ctx, sceneID, "sha256:v1", runtime.HarnessVersion); ok {
		t.Fatalf("re-pushed byte-identical content resurrected a purged validation record")
	}
}
