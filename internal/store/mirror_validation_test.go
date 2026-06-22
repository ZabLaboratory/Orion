package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// seedMirror writes a validated-record seed at the frozen mirror layout
// (canvas/validated/<scene_id>/<bare-64hex>.json) and returns the root.
func seedMirror(t *testing.T, v SceneValidation) string {
	t.Helper()
	root := t.TempDir()
	bare := bareSceneVersion(v.SceneVersion)
	dir := filepath.Join(root, "canvas", "validated", v.SceneID.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, bare+".json"), body, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return root
}

const testBareHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestMirrorValidator_SeededValidatedIsEligible — a seed with status
// "validated" matching the requested triple is air-eligible (RC-A5 §1).
func TestMirrorValidator_SeededValidatedIsEligible(t *testing.T) {
	id := uuid.New()
	ver := "sha256:" + testBareHex
	root := seedMirror(t, SceneValidation{
		SceneID:        id,
		SceneVersion:   ver,
		HarnessVersion: "1",
		Status:         "validated",
		Report:         json.RawMessage(`{}`),
	})
	mv := MirrorValidator{Root: root}
	ok, err := mv.IsVersionValidated(context.Background(), id, ver, "1")
	if err != nil {
		t.Fatalf("IsVersionValidated: %v", err)
	}
	if !ok {
		t.Fatal("seeded validated record must be eligible")
	}
}

// TestMirrorValidator_NoSeedIsNotEligible — a missing seed is (false, nil):
// not eligible, NOT an error (fail-closed at the caller, RC-A5 §2).
func TestMirrorValidator_NoSeedIsNotEligible(t *testing.T) {
	mv := MirrorValidator{Root: t.TempDir()}
	ok, err := mv.IsVersionValidated(context.Background(), uuid.New(), "sha256:"+testBareHex, "1")
	if err != nil {
		t.Fatalf("missing seed must not error: %v", err)
	}
	if ok {
		t.Fatal("missing seed must not be eligible")
	}
}

// TestMirrorValidator_FailedStatusIsNotEligible — a seed present but with a
// non-"validated" status is not eligible (RC-A5 §2).
func TestMirrorValidator_FailedStatusIsNotEligible(t *testing.T) {
	id := uuid.New()
	ver := "sha256:" + testBareHex
	root := seedMirror(t, SceneValidation{
		SceneID:        id,
		SceneVersion:   ver,
		HarnessVersion: "1",
		Status:         "failed",
		Report:         json.RawMessage(`{}`),
	})
	mv := MirrorValidator{Root: root}
	ok, err := mv.IsVersionValidated(context.Background(), id, ver, "1")
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
	id := uuid.New()
	ver := "sha256:" + testBareHex
	root := seedMirror(t, SceneValidation{
		SceneID:        id,
		SceneVersion:   ver,
		HarnessVersion: "1",
		Status:         "validated",
		Report:         json.RawMessage(`{}`),
	})
	mv := MirrorValidator{Root: root}
	// File is found by (id, bare-hex) but its harness_version mismatches → not eligible.
	ok, err := mv.IsVersionValidated(context.Background(), id, ver, "2")
	if err != nil {
		t.Fatalf("IsVersionValidated: %v", err)
	}
	if ok {
		t.Fatal("a seed for another harness_version must not be eligible")
	}
}

// TestMirrorValidator_MismatchedRecordIsNotEligible — a stale/mis-seeded file
// whose internal scene_version does not match the requested version fails
// closed (never airs on a record that does not match the exact triple).
func TestMirrorValidator_MismatchedRecordIsNotEligible(t *testing.T) {
	id := uuid.New()
	ver := "sha256:" + testBareHex
	// Seed a record at the right PATH but with a DIFFERENT internal scene_version.
	root := t.TempDir()
	dir := filepath.Join(root, "canvas", "validated", id.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, _ := json.Marshal(SceneValidation{
		SceneID:        id,
		SceneVersion:   "sha256:deadbeef" + testBareHex[8:],
		HarnessVersion: "1",
		Status:         "validated",
	})
	if err := os.WriteFile(filepath.Join(dir, testBareHex+".json"), body, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	mv := MirrorValidator{Root: root}
	ok, err := mv.IsVersionValidated(context.Background(), id, ver, "1")
	if err != nil {
		t.Fatalf("IsVersionValidated: %v", err)
	}
	if ok {
		t.Fatal("a record whose scene_version mismatches the request must not be eligible")
	}
}

// TestMirrorValidator_MalformedSeedErrors — an unreadable/corrupt seed
// propagates an error so the caller fails closed (RC-A5 §2).
func TestMirrorValidator_MalformedSeedErrors(t *testing.T) {
	id := uuid.New()
	ver := "sha256:" + testBareHex
	root := t.TempDir()
	dir := filepath.Join(root, "canvas", "validated", id.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, testBareHex+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	mv := MirrorValidator{Root: root}
	if _, err := mv.IsVersionValidated(context.Background(), id, ver, "1"); err == nil {
		t.Fatal("a malformed seed must propagate an error (fail-closed)")
	}
}
