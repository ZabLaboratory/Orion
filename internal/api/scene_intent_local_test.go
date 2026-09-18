package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func TestPostLocalAtomicSceneIntent_UsesCachedCapsuleWithoutWorkloadTicket(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelopeBody, _ := canvasEnvelope(program)
	var envelope resolvedSceneEnvelope
	if err := json.Unmarshal(envelopeBody, &envelope); err != nil {
		t.Fatal(err)
	}
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, time.Now(), "scene-1", envelope.BlueProgramDigest)
	intent, err := json.Marshal(sceneIntentRequest{
		IntentID:          "local-intent-1",
		StreamID:          "stream-1",
		Action:            string(attestation.ActionPreparePreview),
		ResolvedSceneRef:  ref,
		BlueProgram:       envelope.BlueProgram,
		BlueProgramDigest: envelope.BlueProgramDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(localAtomicSceneIntentRequest{
		SceneID: "stream-1",
		Intent:  intent,
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Host:          bluehost.NewHost(),
		EmbeddedLocal: true,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent/atomic", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	rec := httptest.NewRecorder()
	postLocalAtomicSceneIntent(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("local atomic scene-intent: got %d: %s", rec.Code, rec.Body.String())
	}
	var response sceneIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "prepared" || response.SceneID != "scene-1" {
		t.Fatalf("unexpected local result: %+v", response)
	}
}

func TestPostLocalAtomicSceneIntent_ReadsContentAddressedArtifacts(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	_, digest := canvasEnvelope(program)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "artifacts", ""), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scene-index", ""), 0o700); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(root, "artifacts", strings.TrimPrefix(digest, "sha256:")+".bin")
	if err := os.WriteFile(artifactPath, program, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scene-index", "scene-1--rev-1.json"), []byte(`{"scene_id":"scene-1","revision_id":"rev-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, time.Now(), "scene-1", digest)
	intent, err := json.Marshal(sceneIntentRequest{
		IntentID:         "local-cache-intent-1",
		StreamID:         "stream-1",
		Action:           string(attestation.ActionPreparePreview),
		ResolvedSceneRef: ref,
		LocalArtifacts:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(localAtomicSceneIntentRequest{SceneID: "stream-1", Intent: intent})
	if err != nil {
		t.Fatal(err)
	}
	deps := SceneIntentDeps{
		Trust:             attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix:     "scenes/",
		OwnerID:           "owner-1",
		TenantID:          "tenant-1",
		LocalArtifactRoot: root,
		Host:              bluehost.NewHost(),
		EmbeddedLocal:     true,
	}
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent/atomic", bytes.NewReader(body))
		req.Header.Set("X-Authenticated-User", "operator-1")
		req.Header.Set("X-Authenticated-Role", "operator")
		rec := httptest.NewRecorder()
		postLocalAtomicSceneIntent(deps)(rec, req)
		return rec
	}
	rec := send()
	if rec.Code != http.StatusOK {
		t.Fatalf("local cache atomic scene-intent: got %d: %s", rec.Code, rec.Body.String())
	}
	var response sceneIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "prepared" || response.SceneID != "scene-1" {
		t.Fatalf("unexpected local cache result: %+v", response)
	}
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	refreshed := send()
	if refreshed.Code != http.StatusConflict {
		t.Fatalf("scene load must revalidate local artifacts, not hide a missing artifact with a retained instance: got %d: %s", refreshed.Code, refreshed.Body.String())
	}
	if err := os.WriteFile(artifactPath, program, 0o600); err != nil {
		t.Fatal(err)
	}
	// Editable Preview has an independent clone on the same wire. Keeping
	// the Blue instance loaded does not mean its scene is still selected.
	activeScene := "editable-scene"
	deps.MirrorFor = func(string, string, bluehost.Slot, []byte) runtime.SceneMirror { return &recordingMirror{} }
	deps.Bridges = bluewire.NewRegistry()
	defer deps.Bridges.Stop(bluehost.SlotPreview)
	deps.ProjectionInterval = time.Hour
	deps.Activate = func(sceneID, _ string, slot bluehost.Slot) {
		if slot != bluehost.SlotPreview {
			t.Fatalf("reactivation reached non-preview slot: %s", slot)
		}
		activeScene = sceneID
	}
	returned := send()
	if returned.Code != http.StatusOK || activeScene != "scene-1" {
		t.Fatalf("warm Blue return acknowledged without switching the preview wire: status=%d active=%s", returned.Code, activeScene)
	}
}
