package compiler

import (
	"crypto/sha256"
	"encoding/hex"
)

// EnvelopeFingerprint is a deterministic content address of the compile
// INPUTS of a regular (non-rollback) push: the scene id, the canvas
// version, the normalised blueprint list, the component refs, and the
// optional LSML bundle hash. Two pushes whose fingerprints match compile
// to a byte-identical artefact (same scene_version), so the second can
// skip Compile and every upstream Canvas/Blue fetch and reuse the first's
// pushed version (the switch-fix idempotence — a go-live pushes twice,
// push → validate → re-push, and each compile re-fetches over the WAN).
//
// It is NOT the scene_version: scene_version hashes the OUTPUT (graph +
// bundle) and is only knowable after a compile; the fingerprint hashes the
// INPUT and is knowable before one. Legacy blueprints resolved by
// current_version are addressed by id alone, so a fingerprint match assumes
// Blue has not advanced that blueprint's current_version between the two
// pushes — true within a single push→validate→re-push cycle (the 3 s
// double-compile this kills). A rollback envelope has no meaningful
// fingerprint (it recompiles nothing); callers must only fingerprint a
// regular push. Blueprints are normalised (sorted by key) so the
// fingerprint is stable regardless of authored order, exactly as
// scene_version is.
func EnvelopeFingerprint(sceneID string, e PushEnvelope) (string, error) {
	refs, err := NormalizeBlueprints(e)
	if err != nil {
		return "", err
	}
	// The canonical, order-stable projection of every input the compile
	// output depends on. RollbackTo is excluded on purpose (a fingerprint
	// is only computed for a regular push).
	payload := struct {
		SceneID       string         `json:"scene_id"`
		CanvasVersion string         `json:"canvas_version"`
		Blueprints    []BlueprintRef `json:"blueprints"`
		Components    []ComponentRef `json:"components"`
		LSMLHash      string         `json:"lsml_bundle_hash"`
	}{
		SceneID:       sceneID,
		CanvasVersion: e.CanvasVersion,
		Blueprints:    refs,
		Components:    e.Components,
		LSMLHash:      e.LSMLBundleHash,
	}
	canon, err := canonicalJSON(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
