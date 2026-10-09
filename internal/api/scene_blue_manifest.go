package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"io"
)

// A manifest preserves Canvas declarations/closure beside its signed executable.
// Its canonical digest checks transport integrity; it is not another signature.
// Legacy capsules without the manifest can execute their signed program, but
// cannot claim to contain a complete authoring closure.
func verifySceneBlueManifest(raw json.RawMessage, claims *attestation.Claims, sourceBytes []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("BLUE_MANIFEST_INVALID")
	}
	readiness, _ := value["readiness"].(map[string]any)
	_, closure := value["binding_closure"].([]any)
	_, declarations := value["declarations"].(map[string]any)
	_, validation := value["validation"].(map[string]any)
	revision, number := value["scene_revision"].(json.Number)
	integer, revisionErr := revision.Int64()
	// Canvas addresses the source LSML separately from its signed artifact set.
	// Older source-less envelopes retain their existing artifact-set identity.
	version := claims.ArtifactSetDigest
	if len(sourceBytes) > 0 {
		var source struct {
			SceneID string `json:"scene_id"`
			Version string `json:"scene_version"`
		}
		if json.Unmarshal(sourceBytes, &source) != nil || source.SceneID != claims.SceneID || source.Version == "" {
			return nil, errors.New("BLUE_MANIFEST_SOURCE_IDENTITY_INVALID")
		}
		version = source.Version
	}
	if value["schema_version"] != "zabcanvas.scene-blue-manifest.v1" || value["scene_id"] != claims.SceneID || value["scene_version"] != version || value["revision_id"] != claims.RevisionID || !number || revisionErr != nil || integer < 1 || !closure || !declarations || !validation || readiness == nil {
		return nil, errors.New("BLUE_MANIFEST_IDENTITY_INVALID")
	}
	for _, field := range []string{"program_ready", "binding_closure_complete", "offline_ready"} {
		if _, ok := readiness[field].(bool); !ok {
			return nil, errors.New("BLUE_MANIFEST_READINESS_INVALID")
		}
	}
	missingBindings, bindings := readiness["missing_binding_ids"].([]any)
	missingKeys, keys := readiness["missing_scene_blueprint_keys"].([]any)
	if !bindings || !keys || readiness["offline_ready"] == true && (readiness["binding_closure_complete"] != true || len(missingBindings) != 0 || len(missingKeys) != 0) {
		return nil, errors.New("BLUE_MANIFEST_READINESS_INVALID")
	}
	expected, _ := value["manifest_digest"].(string)
	delete(value, "manifest_digest")
	actual, err := canonical.Digest(value)
	if err != nil || actual != expected {
		return nil, errors.New("BLUE_MANIFEST_DIGEST_MISMATCH")
	}
	return append([]byte(nil), raw...), nil
}
