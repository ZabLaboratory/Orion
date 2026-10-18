package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func manifestForTest(t *testing.T, claims *attestation.Claims) json.RawMessage {
	t.Helper()
	value := map[string]any{"schema_version": "zabcanvas.scene-blue-manifest.v1", "scene_id": claims.SceneID, "scene_version": claims.ArtifactSetDigest, "scene_revision": json.Number("1"), "revision_id": claims.RevisionID, "declarations": map[string]any{"blueprints": []any{"blue-1"}}, "validation": map[string]any{"status": "validated"}, "binding_closure": []any{map[string]any{"binding_id": "blue-1", "source_digest": "immutable-blue-source"}}, "readiness": map[string]any{"program_ready": true, "offline_ready": true, "binding_closure_complete": true, "missing_binding_ids": []any{}, "missing_scene_blueprint_keys": []any{}}}
	digest, err := canonical.Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	value["manifest_digest"] = digest
	raw, _ := json.Marshal(value)
	return raw
}

func TestLocalSceneIndexPreservesCanvasBlueManifest(t *testing.T) {
	claims := &attestation.Claims{SceneID: "scene-1", RevisionID: "rev-1", ArtifactSetDigest: "sha256:source"}
	manifest := manifestForTest(t, claims)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scene-index"), 0700); err != nil {
		t.Fatal(err)
	}
	index, _ := json.Marshal(localSceneIndex{SceneID: claims.SceneID, RevisionID: claims.RevisionID, BlueManifest: manifest})
	if err := os.WriteFile(filepath.Join(root, "scene-index", "scene-1--rev-1.json"), index, 0600); err != nil {
		t.Fatal(err)
	}
	envelope, err := loadLocalSceneEnvelope(root, claims)
	if err != nil || !bytes.Equal(envelope.BlueManifest, manifest) {
		t.Fatal("local source preparation dropped Canvas Blue closure", err)
	}
	if _, err := verifySceneBlueManifest(envelope.BlueManifest, claims, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSceneCatalogPreservesClosureAndReverifiesAfterReopen(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	source := []byte(`{"lsml":"1.2","scene_id":"scene-1","scene_version":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","layout":{"type":"frame"},"defaults":{}}`)
	raw, digest := canvasEnvelopeWithBundle(minimalProgram(t), source)
	var envelope resolvedSceneEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionTakeOnAir, time.Now(), "scene-1", digest)
	trust := attestation.TrustSet{"canvas-key-1": pub}
	options := attestation.Options{Principal: "operator-1", OwnerID: "owner-1", TenantID: "tenant-1", StreamID: "stream-1", Action: attestation.ActionTakeOnAir, LocatorPrefix: "scenes/"}
	claims, err := attestation.Verify(ref, trust, options)
	if err != nil {
		t.Fatal(err)
	}
	envelope.BlueManifest = manifestForTest(t, claims)
	root := filepath.Join(t.TempDir(), "catalog")
	catalog, err := OpenSceneCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	req := sceneIntentRequest{StreamID: "stream-1", Action: "take-on-air", ResolvedSceneRef: ref, BlueProgram: envelope.BlueProgram, BlueProgramDigest: envelope.BlueProgramDigest, LSMLBundle: envelope.LSMLBundle, LSMLBundleDigest: envelope.LSMLBundleDigest, BlueManifest: envelope.BlueManifest}
	deps := SceneIntentDeps{EmbeddedLocal: true, Catalog: catalog, Trust: trust, OwnerID: "owner-1", TenantID: "tenant-1", LocatorPrefix: "scenes/"}
	prepare := func(request sceneIntentRequest) int {
		raw, _ := json.Marshal(request)
		r := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
		r.Header.Set("X-Authenticated-User", "operator-1")
		r.Header.Set("X-Authenticated-Role", "operator")
		response := httptest.NewRecorder()
		postSceneCatalog(deps)(response, r)
		return response.Code
	}
	if code := prepare(req); code != 200 {
		t.Fatalf("admission %d", code)
	}
	catalog, err = OpenSceneCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := catalog.get("operator-1", claims.SceneID, claims.ArtifactSetDigest, "stream-1", "take-on-air")
	if err != nil || !bytes.Equal(restored.BlueManifest, req.BlueManifest) {
		t.Fatalf("closure lost: %v", err)
	}
	artifacts, err := verifySceneArtifacts(resolvedSceneEnvelope{BlueManifest: restored.BlueManifest, BlueProgram: restored.BlueProgram, BlueProgramDigest: restored.BlueProgramDigest, LSMLBundle: restored.LSMLBundle, LSMLBundleDigest: restored.LSMLBundleDigest}, claims, nil)
	if err != nil || !bytes.Equal(artifacts.Source, source) || len(artifacts.BlueManifest) == 0 {
		t.Fatal("immutable capsule failed replay", err)
	}
	for _, key := range []struct{ principal, action, version string }{{"another-user", "take-on-air", claims.ArtifactSetDigest}, {"operator-1", "prepare-preview", claims.ArtifactSetDigest}, {"operator-1", "take-on-air", "different"}} {
		if _, err := catalog.get(key.principal, claims.SceneID, key.version, "stream-1", key.action); err == nil {
			t.Fatal("catalog crossed authority boundary")
		}
	}
	bad := req
	bad.BlueManifest = bytes.Replace(req.BlueManifest, []byte("immutable-blue-source"), []byte("tampered-blue-source"), 1)
	if code := prepare(bad); code == 200 {
		t.Fatal("corrupt closure admitted")
	}
	bad = req
	bad.ResolvedSceneRef = signedRef(t, priv, "canvas-key-1", attestation.ActionTakeOnAir, time.Now().Add(-time.Hour), "scene-1", digest)
	if code := prepare(bad); code != 403 {
		t.Fatalf("expired authority accepted: %d", code)
	}
	// A catalog entry is private immutable authoring data, never an observation.
	files, _ := os.ReadDir(root)
	if len(files) != 1 {
		t.Fatal("unexpected catalog writes")
	}
}

func TestSceneControlFileValidationAndDesiredOnlyPersistence(t *testing.T) {
	catalog, _ := OpenSceneCatalog(filepath.Join(t.TempDir(), "catalog"))
	path := filepath.Join(t.TempDir(), "selection.lsml")
	deps := SceneIntentDeps{EmbeddedLocal: true, Catalog: catalog}
	controller, err := OpenSceneControl(deps, nil, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller.desired["program"] = SceneSelection{SceneID: "scene-1", Version: "sha256:version", StreamID: "stream-1"}
	if err = controller.persist(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSceneControl(deps, nil, path, nil)
	if err != nil || reopened.desired["program"].SceneID != "scene-1" {
		t.Fatal("desired selection lost", err)
	}
	for _, raw := range []string{`{}`, `{"lsml":"1.2","schema":"wrong"}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = OpenSceneControl(deps, nil, path, nil); err == nil {
			t.Fatal("corrupt control admitted")
		}
	}
}
