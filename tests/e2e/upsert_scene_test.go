//go:build e2e

package e2e

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/store"
)

// TestE2E_UpsertScene_FirstPushCreatesRow proves ADR 002 §3.1 /
// criterion 1: UpsertScene on a scene whose row NEVER existed in Orion
// creates it — status=active, name=placeholder (scene_id), pointer
// NULL. This is the first-push path Canvas drives without ever seeding
// Orion. The FK scene_definitions.scene_id→scenes(id) is now
// satisfiable.
func TestE2E_UpsertScene_FirstPushCreatesRow(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	id := uuid.New()

	// Pre-condition: the row genuinely does not exist (no CreateScene).
	if _, err := st.GetScene(ctx, id); err == nil {
		t.Fatalf("scene unexpectedly pre-existed")
	}

	sc, err := st.UpsertScene(ctx, id, id.String())
	if err != nil {
		t.Fatalf("UpsertScene first push: %v", err)
	}
	if sc.ID != id {
		t.Fatalf("returned id = %s, want %s", sc.ID, id)
	}
	if sc.Status != store.SceneActive {
		t.Fatalf("status = %q, want active", sc.Status)
	}
	if sc.Name != id.String() {
		t.Fatalf("name = %q, want placeholder %q (Canvas owns the canonical name)", sc.Name, id.String())
	}
	if sc.LatestPushedVersion != nil {
		t.Fatalf("latest_pushed_version = %v, want NULL until first push fills it", *sc.LatestPushedVersion)
	}

	// The row is now readable — FK target exists.
	if _, err := st.GetScene(ctx, id); err != nil {
		t.Fatalf("GetScene after upsert: %v", err)
	}
}

// TestE2E_UpsertScene_IdempotentPreservesRow proves criterion 3: a
// second upsert (subsequent push) is a no-op — it does NOT reset name,
// status, or the latest_pushed_version pointer. We seed a row with an
// operator/Canvas-set name and an advanced pointer (a pushed scene),
// then upsert with a DIFFERENT placeholder name and assert nothing
// moved.
func TestE2E_UpsertScene_IdempotentPreservesRow(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	id := uuid.New()

	// Seed an existing scene with a human name (as if Canvas-named or
	// operator-set) and advance the pointer to mimic a pushed scene.
	if _, err := st.CreateScene(ctx, id, "Operator Named Scene"); err != nil {
		t.Fatal(err)
	}
	ptr := "scene-version-abc"
	if err := st.Tx(ctx, func(tx store.Tx) error {
		return st.SetLatestPushedVersion(ctx, tx, id, &ptr)
	}); err != nil {
		t.Fatal(err)
	}

	// Re-upsert with a placeholder name — must NOT overwrite anything.
	sc, err := st.UpsertScene(ctx, id, id.String())
	if err != nil {
		t.Fatalf("UpsertScene idempotent: %v", err)
	}
	if sc.Name != "Operator Named Scene" {
		t.Fatalf("name was overwritten to %q — upsert must preserve the existing name (criterion 3)", sc.Name)
	}
	if sc.Status != store.SceneActive {
		t.Fatalf("status = %q, want active preserved", sc.Status)
	}
	if sc.LatestPushedVersion == nil || *sc.LatestPushedVersion != ptr {
		t.Fatalf("latest_pushed_version = %v, want %q preserved (criterion 3)", sc.LatestPushedVersion, ptr)
	}
}

// TestE2E_UpsertScene_ArchivedReturnsArchivedRow proves criterion 5:
// upserting an archived scene does NOT resurrect it — DO UPDATE SET
// id=id never touches status — so the returned row still reports
// archived and the push handler's guard rejects with 409.
func TestE2E_UpsertScene_ArchivedReturnsArchivedRow(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	id := uuid.New()

	if _, err := st.CreateScene(ctx, id, "to-archive"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSceneStatus(ctx, id, store.SceneArchived); err != nil {
		t.Fatal(err)
	}

	sc, err := st.UpsertScene(ctx, id, id.String())
	if err != nil {
		t.Fatalf("UpsertScene on archived: %v", err)
	}
	if sc.Status != store.SceneArchived {
		t.Fatalf("status = %q, want archived preserved — upsert must not resurrect (criterion 5)", sc.Status)
	}
}

// TestE2E_UpsertScene_ConcurrentFirstPushRaceSafe proves criterion 4 /
// R2: N concurrent first-pushes of the SAME id all succeed (one
// inserts, the rest no-op via ON CONFLICT … DO UPDATE … RETURNING),
// exactly one row exists, and no duplicate-key / FK error surfaces.
func TestE2E_UpsertScene_ConcurrentFirstPushRaceSafe(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	id := uuid.New()

	const racers = 8
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.UpsertScene(ctx, id, id.String()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent first-push raced: %v (R2 — ON CONFLICT must make this safe)", err)
	}

	// Exactly one row, readable.
	sc, err := st.GetScene(ctx, id)
	if err != nil {
		t.Fatalf("GetScene after race: %v", err)
	}
	if sc.ID != id {
		t.Fatalf("row id = %s, want %s", sc.ID, id)
	}
}
