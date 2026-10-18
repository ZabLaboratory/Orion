package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Only admitted immutable source/program capsules. Live mirror state and host
// credentials are never persisted here; each replay rechecks the signature.
type SceneCatalog struct {
	mu   sync.Mutex
	root string
}
type catalogEntry struct {
	Principal string             `json:"principal"`
	SceneID   string             `json:"scene_id"`
	Version   string             `json:"scene_version"`
	StreamID  string             `json:"stream_id"`
	Action    string             `json:"action"`
	Intent    sceneIntentRequest `json:"intent"`
}

func OpenSceneCatalog(root string) (*SceneCatalog, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("SCENE_CATALOG_ABSOLUTE_PATH_REQUIRED")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &SceneCatalog{root: root}, nil
}
func catalogKey(principal, sceneID, version, streamID, action string) string {
	raw, _ := json.Marshal([]string{principal, sceneID, version, streamID, action})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (c *SceneCatalog) put(principal string, claims *attestation.Claims, req sceneIntentRequest, envelope resolvedSceneEnvelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Normalize a Prism-cache reference to immutable admitted bytes, allowing
	// recovery after that external cache has been moved/removed. Never copy it.
	req.LocalArtifacts = false
	req.BlueProgram = envelope.BlueProgram
	req.BlueProgramDigest = envelope.BlueProgramDigest
	req.LSMLBundle = envelope.LSMLBundle
	req.LSMLBundleDigest = envelope.LSMLBundleDigest
	req.RenderBundle = envelope.RenderBundle
	req.RenderBundleDigest = envelope.RenderBundleDigest
	req.BlueManifest = append(json.RawMessage(nil), envelope.BlueManifest...)
	if envelope.blueProgramBytes != nil {
		req.BlueProgram = base64.StdEncoding.EncodeToString(envelope.blueProgramBytes)
	}
	if envelope.lsmlBundleBytes != nil {
		req.LSMLBundle = base64.StdEncoding.EncodeToString(envelope.lsmlBundleBytes)
	}
	if envelope.renderBundleBytes != nil {
		req.RenderBundle = base64.StdEncoding.EncodeToString(envelope.renderBundleBytes)
	}
	entry := catalogEntry{Principal: principal, SceneID: claims.SceneID, Version: claims.ArtifactSetDigest, StreamID: req.StreamID, Action: req.Action, Intent: req}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if len(raw) > maxSceneIntentBytes {
		return errors.New("SCENE_CATALOG_CAPSULE_LIMIT")
	}
	filename := catalogKey(principal, entry.SceneID, entry.Version, entry.StreamID, entry.Action) + ".json"
	files, err := os.ReadDir(c.root)
	if err != nil {
		return err
	}
	count := 0
	var size int64
	for _, file := range files {
		if filepath.Ext(file.Name()) != ".json" || file.Name() == filename {
			continue
		}
		info, err := file.Info()
		if err != nil {
			return err
		}
		count++
		size += info.Size()
	}
	if count >= 64 || size+int64(len(raw)) > 64<<20 {
		return errors.New("SCENE_CATALOG_QUOTA")
	}
	return writeControlFile(filepath.Join(c.root, filename), raw)
}
func (c *SceneCatalog) get(principal, sceneID, version, streamID, action string) (sceneIntentRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	file, err := os.Open(filepath.Join(c.root, catalogKey(principal, sceneID, version, streamID, action)+".json"))
	if err != nil {
		return sceneIntentRequest{}, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxSceneIntentBytes+1))
	if err != nil || len(raw) > maxSceneIntentBytes {
		return sceneIntentRequest{}, errors.New("SCENE_CATALOG_INVALID")
	}
	var entry catalogEntry
	if json.Unmarshal(raw, &entry) != nil || entry.Principal != principal || entry.SceneID != sceneID || entry.Version != version || entry.StreamID != streamID || entry.Action != action {
		return sceneIntentRequest{}, errors.New("SCENE_CATALOG_IDENTITY_MISMATCH")
	}
	return entry.Intent, nil
}

// postSceneCatalog prepares bytes without taking/stepping a scene. Admission is
// identical to the embedded-local path: same principal/action/ref and verifier.
func postSceneCatalog(deps SceneIntentDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if !deps.EmbeddedLocal || deps.Catalog == nil {
			writeOperatorError(w, 409, "SCENE_CATALOG_UNAVAILABLE", "local scene catalog unavailable")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxSceneIntentBytes+1))
		var req sceneIntentRequest
		if err != nil || len(raw) > maxSceneIntentBytes || json.Unmarshal(raw, &req) != nil {
			writeOperatorError(w, 400, "MALFORMED_INTENT", "invalid capsule")
			return
		}
		if req.Action != string(attestation.ActionPreparePreview) && req.Action != string(attestation.ActionTakeOnAir) {
			writeOperatorError(w, 400, "UNKNOWN_ACTION", "invalid scene action")
			return
		}
		if req.LocalArtifacts && (req.BlueProgram != "" || req.LSMLBundle != "" || req.RenderBundle != "") {
			writeOperatorError(w, 400, "MALFORMED_INTENT", "local and inline artifacts cannot be combined")
			return
		}
		principal := authSource.FromHeaders(r.Header).UserID
		claims, err := attestation.Verify(req.ResolvedSceneRef, deps.Trust, attestation.Options{Principal: principal, OwnerID: deps.OwnerID, TenantID: deps.TenantID, StreamID: req.StreamID, Action: attestation.Action(req.Action), LocatorPrefix: deps.LocatorPrefix, ClockSkew: deps.AttestationClockSkew})
		if err != nil {
			writeOperatorError(w, 403, "ATTESTATION_REJECTED", err.Error())
			return
		}
		envelope := resolvedSceneEnvelope{BlueManifest: req.BlueManifest, BlueProgram: req.BlueProgram, BlueProgramDigest: req.BlueProgramDigest, LSMLBundle: req.LSMLBundle, LSMLBundleDigest: req.LSMLBundleDigest, RenderBundle: req.RenderBundle, RenderBundleDigest: req.RenderBundleDigest}
		if req.LocalArtifacts {
			envelope, err = loadLocalSceneEnvelope(deps.LocalArtifactRoot, claims)
		}
		if err == nil {
			_, err = verifySceneArtifacts(envelope, claims, deps.ProgramVerificationCache)
		}
		if err != nil {
			writeOperatorError(w, 400, "CANVAS_ARTIFACT_DIGEST_MISMATCH", err.Error())
			return
		}
		if err = deps.Catalog.put(principal, claims, req, envelope); err != nil {
			writeOperatorError(w, 409, "SCENE_CATALOG_WRITE_FAILED", err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"status": "prepared", "scene_id": claims.SceneID, "scene_version": claims.ArtifactSetDigest, "execution": "signed-program-reverified-on-selection"})
	})
}

func writeControlFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".scene-control-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = io.Copy(file, bytes.NewReader(raw))
	}
	if err == nil {
		err = file.Sync()
	}
	closed := file.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	return os.Rename(name, path)
}
