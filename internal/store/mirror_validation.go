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
// The mirror is seeded by Prism / the ZabCanvas export (#144/#145) from the
// authoritative published scene. Its layout, FROZEN by that export:
//
//	<Root>/canvas/validated/<scene_id>/<bare-64hex>.json
//
// CONTRACT — the lookup KEY is the canvas_version, not Orion's compiled
// scene_version (Conduit A1, 2026-06-22). The producer of the seed is
// OUTSIDE Orion (the ZabCanvas export / Prism push), and a read-only export
// CANNOT compute Orion's compiled scene_version (that needs a live compile).
// The only address the producer and the gate both know at push time is the
// canvas_version — the layout content address Prism pushes
// (`PushEnvelope.canvas_version`) and the gateway sidecar serves at
// `GET /canvas/api/v1/layouts/<canvas_version>`. So in embedded-local the
// gate resolves the scene's canvas_version from the latest pushed definition
// and looks the seed up by THAT, while the antenne path (storeAirValidator,
// PG row keyed by the compiled scene_version) is unchanged (RC-A1 parity:
// only the embedded-local source diverges, the air-eligibility decision is
// the same). The seed file is therefore keyed `<bare canvas_version>.json`
// and its `scene_version` field carries the same canvas_version (prefixed
// "sha256:").
//
// The on-disk shape is snake_case JSON ({scene_id, scene_version,
// harness_version, status, report}) — the convention the ZabCanvas producer
// writes and store.SceneValidation now carries via json tags (validations.go).
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

	// Store resolves the scene's canvas_version from its latest pushed
	// definition (the lookup key, per the contract above). The embedded-local
	// store (SQLite, #222) — the same store the rest of the runtime reads.
	Store Store
}

// scenePrefix is the canonical scene_version prefix (mirrors
// compiler.computeSceneVersion, which formats "sha256:<hex>").
const scenePrefix = "sha256:"

// bareSceneVersion strips the canonical "sha256:" prefix to yield the bare
// 64-hex content address the mirror filenames use. A version without the
// prefix is returned unchanged (defensive).
func bareSceneVersion(sceneVersion string) string {
	return strings.TrimPrefix(sceneVersion, scenePrefix)
}

// prefixedSceneVersion ensures the canonical "sha256:" prefix on a version
// (the record's scene_version field carries the prefixed form).
func prefixedSceneVersion(sceneVersion string) string {
	if strings.HasPrefix(sceneVersion, scenePrefix) {
		return sceneVersion
	}
	return scenePrefix + sceneVersion
}

// validatedPath builds the on-disk path of the validated record for a
// (scene, canvas_version), per the frozen mirror layout.
func (m MirrorValidator) validatedPath(sceneID uuid.UUID, canvasVersion string) string {
	return filepath.Join(m.Root, "canvas", "validated", sceneID.String(), bareSceneVersion(canvasVersion)+".json")
}

// canvasVersion resolves the scene's canvas_version from its latest pushed
// definition — the lookup key (see MirrorValidator contract). A scene with
// no pushed version, or whose definition cannot be read, is not eligible
// (fail-closed at the caller): the gate already rejected SCENE_NOT_PUSHED
// upstream, so a miss here is a mis-seeded / inconsistent local store.
func (m MirrorValidator) canvasVersion(ctx context.Context, sceneID uuid.UUID) (string, error) {
	pv, err := m.Store.GetLatestPushedVersion(ctx, sceneID)
	if err != nil {
		return "", err
	}
	def, err := m.Store.GetDefinition(ctx, pv.DefinitionID)
	if err != nil {
		return "", err
	}
	return def.CanvasVersion, nil
}

// IsVersionValidated reports whether the mirror carries a `validated`
// record for sceneID at harnessVersion, keyed by the scene's canvas_version
// (the contract key). Same signature and posture as Store.IsVersionValidated
// so the gate seam is transport-blind. The compiled scene_version arg (the
// third positional, named in the interface) is intentionally IGNORED here —
// embedded-local keys on the canvas_version the producer can address, not the
// compiled hash — hence the `_`.
func (m MirrorValidator) IsVersionValidated(ctx context.Context, sceneID uuid.UUID, _, harnessVersion string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	canvasVer, err := m.canvasVersion(ctx, sceneID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// No pushed version for this scene → not eligible, not an error
			// (parity with a missing DB row, fail-closed at the caller).
			return false, nil
		}
		return false, fmt.Errorf("mirror validation resolve canvas_version for %s: %w", sceneID, err)
	}
	path := m.validatedPath(sceneID, canvasVer)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No seed for this (scene, canvas_version) → not eligible, not an
			// error (parity with a missing DB row, fail-closed at the caller).
			return false, nil
		}
		return false, fmt.Errorf("mirror validation read %s: %w", path, err)
	}
	var v SceneValidation
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, fmt.Errorf("mirror validation parse %s: %w", path, err)
	}
	// Fail-closed identity check: the seed must be the record it claims to
	// be. The record's scene_id and scene_version (== the canvas_version it
	// is keyed by) and harness_version must match — a stale or mis-seeded
	// file (wrong identity) is NOT eligible. We never air on a record that
	// does not match the exact triple the gate resolved.
	if v.SceneID != sceneID ||
		prefixedSceneVersion(v.SceneVersion) != prefixedSceneVersion(canvasVer) ||
		v.HarnessVersion != harnessVersion {
		return false, nil
	}
	return v.Status == ValidationValidated, nil
}
