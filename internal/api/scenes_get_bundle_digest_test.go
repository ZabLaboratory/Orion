package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

func TestGetRenderBundle_ProgrammedSceneAcceptsBundleDigest(t *testing.T) {
	host := bluehost.NewHost()
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:program", minimalProgram(t), nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	bundle := []byte(`{"root":{"kind":"scene"}}`)
	host.SetBundle(bluehost.SlotPreview, bundle)
	sum := sha256.Sum256(bundle)
	bundleDigest := "sha256:" + hex.EncodeToString(sum[:])

	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/scene-1/render-bundle?v="+bundleDigest, nil)
	req.SetPathValue("id", "scene-1")
	rec := httptest.NewRecorder()
	getRenderBundle(PublicDeps{SceneIntent: &SceneIntentDeps{Host: host}})(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected bundle digest to resolve programmed scene, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(bundle) {
		t.Fatalf("unexpected bundle body: %s", rec.Body.String())
	}
}
