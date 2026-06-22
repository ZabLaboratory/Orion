package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// The contract (Conduit A1): the mirror seed is keyed by the scene's
// canvas_version (the layout content address the ZabCanvas producer knows at
// push time), NOT Orion's compiled scene_version. The gate resolves the
// canvas_version from the latest pushed definition and looks the seed up by
// it. These two hashes DIFFER in production (compile mints its own
// scene_version), so the tests keep them deliberately distinct.
const (
	testCanvasVersion   = "4b529bb22fb62de7a59f9e875aeded5afc091221acb8cb1434215ac60ee0ae7a"
	testCompiledVersion = "sha256:51d5e3cc89abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// pushScene pushes a scene into the store with the given canvas_version and a
// (distinct) compiled scene_version, so the validator must resolve the
// canvas_version to find the seed. Returns the scene id.
func pushScene(t *testing.T, st Store, canvasVersion, compiledVersion string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	sceneID := uuid.New()
	defID := uuid.New()
	if _, err := st.UpsertScene(ctx, sceneID, "mirror-test"); err != nil {
		t.Fatalf("UpsertScene: %v", err)
	}
	err := st.Tx(ctx, func(tx Tx) error {
		next, err := st.NextDefinitionVersionTx(ctx, tx, sceneID)
		if err != nil {
			return err
		}
		if err := st.InsertDefinitionTx(ctx, tx, SceneDefinition{
			ID: defID, SceneID: sceneID, DefinitionVersion: next,
			CanvasVersion: canvasVersion, BlueBlueprintID: "bp-1",
			ComponentsJSON: json.RawMessage(`[]`),
		}); err != nil {
			return err
		}
		if err := st.InsertPushedVersion(ctx, tx, ScenePushedVersion{
			SceneID: sceneID, SceneVersion: compiledVersion, DefinitionID: defID,
			GraphJSON:  json.RawMessage(`{"nodes":[]}`),
			BundleJSON: json.RawMessage(`{"root":{}}`),
		}); err != nil {
			return err
		}
		v := compiledVersion
		return st.SetLatestPushedVersion(ctx, tx, sceneID, &v)
	})
	if err != nil {
		t.Fatalf("push tx: %v", err)
	}
	return sceneID
}

// seedSnakeCase writes a validated-record seed in the SNAKE_CASE wire form the
// ZabCanvas export (#145) actually produces — the casing the producer writes,
// NOT Go's PascalCase field names — keyed by the bare canvas_version, at the
// frozen mirror layout.
func seedSnakeCase(t *testing.T, root, sceneID, canvasVersion, harness, status string) {
	t.Helper()
	bare := bareSceneVersion(canvasVersion)
	dir := filepath.Join(root, "canvas", "validated", sceneID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	record := map[string]any{
		"scene_id":        sceneID,
		"scene_version":   prefixedSceneVersion(canvasVersion),
		"harness_version": harness,
		"status":          status,
		"report":          map[string]any{},
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, bare+".json"), body, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
}

// TestMirrorValidator_SeededValidatedIsEligible — a snake_case seed keyed by
// the scene's canvas_version, status "validated", matching the harness is
// air-eligible. The gate is passed the COMPILED scene_version and must still
// resolve the canvas_version to find the seed (RC-A5 §1, Conduit A1).
func TestMirrorValidator_SeededValidatedIsEligible(t *testing.T) {
	st := openTestSQLite(t)
	root := t.TempDir()
	id := pushScene(t, st, testCanvasVersion, testCompiledVersion)
	seedSnakeCase(t, root, id.String(), testCanvasVersion, "1", "validated")

	mv := MirrorValidator{Root: root, Store: st}
	ok, err := mv.IsVersionValidated(context.Background(), id, testCompiledVersion, "1")
	if err != nil {
		t.Fatalf("IsVersionValidated: %v", err)
	}
	if !ok {
		t.Fatal("seeded validated record (keyed by canvas_version) must be eligible")
	}
}

// TestMirrorValidator_PascalCaseSeedIsNotSilentlyDropped — the OLD bug: a seed
// written in PascalCase (Go field names) must NOT be silently accepted as a
// zero-value record — its scene_id is empty so the identity check fails. This
// proves the snake_case contract is the one that resolves; a PascalCase file
// is just a mismatched/foreign record.
func TestMirrorValidator_PascalCaseSeedIsNotSilentlyDropped(t *testing.T) {
	st := openTestSQLite(t)
	root := t.TempDir()
	id := pushScene(t, st, testCanvasVersion, testCompiledVersion)

	dir := filepath.Join(root, "canvas", "validated", id.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// PascalCase body — what the tagless struct used to marshal. Under
	// snake_case tags these keys are ignored → zero-value record → not eligible.
	body := []byte(`{"SceneID":"` + id.String() + `","SceneVersion":"` +
		prefixedSceneVersion(testCanvasVersion) + `","HarnessVersion":"1","Status":"validated"}`)
	if err := os.WriteFile(filepath.Join(dir, testCanvasVersion+".json"), body, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	mv := MirrorValidator{Root: root, Store: st}
	ok, err := mv.IsVersionValidated(context.Background(), id, testCompiledVersion, "1")
	if err != nil {
		t.Fatalf("IsVersionValidated: %v", err)
	}
	if ok {
		t.Fatal("a PascalCase seed must not be eligible under the snake_case contract")
	}
}

// TestMirrorValidator_NoSeedIsNotEligible — a pushed scene with no seed file
// is (false, nil): not eligible, NOT an error (fail-closed at the caller).
func TestMirrorValidator_NoSeedIsNotEligible(t *testing.T) {
	st := openTestSQLite(t)
	root := t.TempDir()
	id := pushScene(t, st, testCanvasVersion, testCompiledVersion)

	mv := MirrorValidator{Root: root, Store: st}
	ok, err := mv.IsVersionValidated(context.Background(), id, testCompiledVersion, "1")
	if err != nil {
		t.Fatalf("missing seed must not error: %v", err)
	}
	if ok {
		t.Fatal("missing seed must not be eligible")
	}
}

// TestMirrorValidator_UnpushedSceneIsNotEligible — a scene with no pushed
// version cannot resolve a canvas_version: not eligible, not an error.
func TestMirrorValidator_UnpushedSceneIsNotEligible(t *testing.T) {
	st := openTestSQLite(t)
	mv := MirrorValidator{Root: t.TempDir(), Store: st}
	ok, err := mv.IsVersionValidated(context.Background(), uuid.New(), testCompiledVersion, "1")
	if err != nil {
		t.Fatalf("unpushed scene must not error: %v", err)
	}
	if ok {
		t.Fatal("an unpushed scene must not be eligible")
	}
}

// TestMirrorValidator_FailedStatusIsNotEligible — a seed present but with a
// non-"validated" status is not eligible (RC-A5 §2).
func TestMirrorValidator_FailedStatusIsNotEligible(t *testing.T) {
	st := openTestSQLite(t)
	root := t.TempDir()
	id := pushScene(t, st, testCanvasVersion, testCompiledVersion)
	seedSnakeCase(t, root, id.String(), testCanvasVersion, "1", "failed")

	mv := MirrorValidator{Root: root, Store: st}
	ok, err := mv.IsVersionValidated(context.Background(), id, testCompiledVersion, "1")
	if err != nil {
		t.Fatalf("IsVersionValidated: %v", err)
	}
	if ok {
		t.Fatal("a failed-status seed must not be eligible")
	}
}

// TestMirrorValidator_WrongHarnessIsNotEligible — the gate keys on
// harness_version; a seed for a different harness must not air the request.
func TestMirrorValidator_WrongHarnessIsNotEligible(t *testing.T) {
	st := openTestSQLite(t)
	root := t.TempDir()
	id := pushScene(t, st, testCanvasVersion, testCompiledVersion)
	seedSnakeCase(t, root, id.String(), testCanvasVersion, "1", "validated")

	mv := MirrorValidator{Root: root, Store: st}
	// File is found by (id, bare canvas_version) but its harness_version
	// mismatches → not eligible.
	ok, err := mv.IsVersionValidated(context.Background(), id, testCompiledVersion, "2")
	if err != nil {
		t.Fatalf("IsVersionValidated: %v", err)
	}
	if ok {
		t.Fatal("a seed for another harness_version must not be eligible")
	}
}

// TestMirrorValidator_MismatchedRecordIsNotEligible — a stale/mis-seeded file
// keyed at the right canvas_version path but carrying a DIFFERENT internal
// scene_version fails closed (never airs on a record that does not match).
func TestMirrorValidator_MismatchedRecordIsNotEligible(t *testing.T) {
	st := openTestSQLite(t)
	root := t.TempDir()
	id := pushScene(t, st, testCanvasVersion, testCompiledVersion)

	dir := filepath.Join(root, "canvas", "validated", id.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Right PATH (bare canvas_version), wrong internal scene_version.
	body, _ := json.Marshal(map[string]any{
		"scene_id":        id.String(),
		"scene_version":   "sha256:deadbeef" + testCanvasVersion[8:],
		"harness_version": "1",
		"status":          "validated",
		"report":          map[string]any{},
	})
	if err := os.WriteFile(filepath.Join(dir, testCanvasVersion+".json"), body, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	mv := MirrorValidator{Root: root, Store: st}
	ok, err := mv.IsVersionValidated(context.Background(), id, testCompiledVersion, "1")
	if err != nil {
		t.Fatalf("IsVersionValidated: %v", err)
	}
	if ok {
		t.Fatal("a record whose scene_version mismatches the canvas_version must not be eligible")
	}
}

// TestMirrorValidator_MalformedSeedErrors — an unreadable/corrupt seed
// propagates an error so the caller fails closed (RC-A5 §2).
func TestMirrorValidator_MalformedSeedErrors(t *testing.T) {
	st := openTestSQLite(t)
	root := t.TempDir()
	id := pushScene(t, st, testCanvasVersion, testCompiledVersion)

	dir := filepath.Join(root, "canvas", "validated", id.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, testCanvasVersion+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	mv := MirrorValidator{Root: root, Store: st}
	if _, err := mv.IsVersionValidated(context.Background(), id, testCompiledVersion, "1"); err == nil {
		t.Fatal("a malformed seed must propagate an error (fail-closed)")
	}
}
