package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/providers"
)

// httpRequiresProgram is a program declaring `requires` on core.http.request
// v1/request (internal/bluespike/testdata/02-http-requires.program.json) —
// the fixture proving Prepare/Take through checkProviders (Refs #332).
func httpRequiresProgram(t *testing.T) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("../bluespike/testdata/02-http-requires.program.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestPostSceneIntent_PreparePreview_CapabilityUnavailableWithoutProviders
// reproduces the bug this issue fixes: nil Providers/Policy (the state
// scene_intent.go was in before #332) makes ANY program declaring
// `requires` fail closed at Prepare, even though the attestation/workload
// chain succeeded — a display-only program with no `requires` (the
// existing 01-minimal fixture) is unaffected.
func TestPostSceneIntent_PreparePreview_CapabilityUnavailableWithoutProviders(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := httpRequiresProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
		// Providers/Policy deliberately left nil.
	}

	body, _ := json.Marshal(sceneIntentRequest{
		IntentID:         "intent-1",
		StreamID:         "stream-1",
		Target:           "preview",
		Action:           string(attestation.ActionPreparePreview),
		ResolvedSceneRef: ref,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "opaque-ticket")

	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (CAPABILITY_UNAVAILABLE surfaced as HOST_PREPARE_FAILED) without providers, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPostSceneIntent_PreparePreview_SucceedsWithProviders is the fix's
// proof: the same requires-declaring program succeeds Prepare once deps
// carries the real internal/providers.Registry() + Policy(...).
func TestPostSceneIntent_PreparePreview_SucceedsWithProviders(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := httpRequiresProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
		Providers:     providers.Registry(),
		Policy:        providers.Policy(true),
	}

	body, _ := json.Marshal(sceneIntentRequest{
		IntentID:         "intent-1",
		StreamID:         "stream-1",
		Target:           "preview",
		Action:           string(attestation.ActionPreparePreview),
		ResolvedSceneRef: ref,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "opaque-ticket")

	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp sceneIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "prepared" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestPostSceneIntent_PreparePreview_DeniedWhenHTTPEgressPolicyClosed proves
// Policy is actually consulted: httpEgressAllowed=false must deny the same
// program that succeeds when true (previous test), via
// CAPABILITY_NOT_ALLOWED inside checkProviders.
func TestPostSceneIntent_PreparePreview_DeniedWhenHTTPEgressPolicyClosed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := httpRequiresProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
		Providers:     providers.Registry(),
		Policy:        providers.Policy(false),
	}

	body, _ := json.Marshal(sceneIntentRequest{
		IntentID:         "intent-1",
		StreamID:         "stream-1",
		Target:           "preview",
		Action:           string(attestation.ActionPreparePreview),
		ResolvedSceneRef: ref,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "opaque-ticket")

	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (CAPABILITY_NOT_ALLOWED surfaced as HOST_PREPARE_FAILED) with closed egress policy, got %d: %s", rec.Code, rec.Body.String())
	}
}
