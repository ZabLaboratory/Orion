package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// MirrorValidator reads `validated` records from the embedded-local
// validation mirror on the filesystem instead of a SQL store (ADR 016
// Amendment 1 / issue #247). It is the embedded-local substitute for the
// PG/SQLite IsVersionValidated read: the gate's air-eligibility decision is
// the SAME (a (scene, version) is eligible iff it carries a `validated`
// record for the current harness_version), only the transport changes — a
// seed file read, not a DB row.
//
// The mirror is seeded by Prism (the orion-engine sidecar) from the
// authoritative ZabCanvas export (#144/#145). Its layout, FROZEN by that
// export:
//
//	<Root>/canvas/validated/<scene_id>/<bare-64hex>.json
//
// where <bare-64hex> is the scene_version WITHOUT the "sha256:" prefix, and
// the record inside carries scene_version WITH the prefix. The shape is the
// store.SceneValidation JSON: {scene_id, scene_version, harness_version,
// status, report}.
//
// Posture matches the DB validator exactly (validations.go IsVersionValidated):
//   - no file / missing record  → (false, nil): not eligible, NOT an error
//     (the author must validate; the gate refuses, fail-closed at the caller).
//   - a read/parse error         → (false, err): propagated so the caller
//     fails closed rather than airing an unproven version.
//   - status == "validated" AND the record's identity matches the request
//     → (true, nil).
type MirrorValidator struct {
	// Root is the mirror root directory (ORION_VALIDATION_MIRROR_ROOT) —
	// the directory that CONTAINS `canvas/validated/...`, not the
	// `validated` dir itself.
	Root string
}

// scenePrefix is the canonical scene_version prefix (mirrors
// compiler.computeSceneVersion, which formats "sha256:<hex>").
const scenePrefix = "sha256:"

// bareSceneVersion strips the canonical "sha256:" prefix to yield the bare
// 64-hex content address the mirror filenames use. A version without the
// prefix is returned unchanged (defensive: the gate always passes the
// prefixed form today).
func bareSceneVersion(sceneVersion string) string {
	return strings.TrimPrefix(sceneVersion, scenePrefix)
}

// validatedPath builds the on-disk path of the validated record for a
// (scene, version), per the frozen mirror layout.
func (m MirrorValidator) validatedPath(sceneID uuid.UUID, sceneVersion string) string {
	return filepath.Join(m.Root, "canvas", "validated", sceneID.String(), bareSceneVersion(sceneVersion)+".json")
}

// IsVersionValidated reports whether the mirror carries a `validated`
// record for (sceneID, sceneVersion) at harnessVersion. Same signature and
// posture as Store.IsVersionValidated so the gate seam is transport-blind.
func (m MirrorValidator) IsVersionValidated(ctx context.Context, sceneID uuid.UUID, sceneVersion, harnessVersion string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path := m.validatedPath(sceneID, sceneVersion)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No seed for this (scene, version) → not eligible, not an error
			// (parity with a missing DB row, fail-closed at the caller).
			return false, nil
		}
		return false, fmt.Errorf("mirror validation read %s: %w", path, err)
	}
	var v SceneValidation
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, fmt.Errorf("mirror validation parse %s: %w", path, err)
	}
	// Fail-closed identity check: the seed must be the record it claims to
	// be. A scene_id / scene_version / harness_version mismatch (a stale or
	// mis-seeded file) is NOT eligible — we never air on a record that does
	// not match the exact triple the gate asked about.
	if v.SceneID != sceneID || v.SceneVersion != sceneVersion || v.HarnessVersion != harnessVersion {
		return false, nil
	}
	return v.Status == ValidationValidated, nil
}
