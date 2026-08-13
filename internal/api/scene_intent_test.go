package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/workload"
)

type fakeWorkload struct {
	admitErr error
	mintErr  error
	fetchErr error
	body     json.RawMessage
}

func (f *fakeWorkload) AdmitAuthContext(_ context.Context, ticket string) (*workload.AuthContextAdmission, error) {
	if f.admitErr != nil {
		return nil, f.admitErr
	}
	return &workload.AuthContextAdmission{AdmissionID: "adm-1", IntentID: "intent-1"}, nil
}

func (f *fakeWorkload) MintDelegation(_ context.Context, admission *workload.AuthContextAdmission) (*workload.Delegation, error) {
	if f.mintErr != nil {
		return nil, f.mintErr
	}
	return &workload.Delegation{JTI: "jti-1"}, nil
}

func (f *fakeWorkload) FetchCanvas(_ context.Context, jti string) (*workload.CanvasArtifact, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return &workload.CanvasArtifact{Status: 200, Body: f.body}, nil
}

func minimalProgram(t *testing.T) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("../bluespike/testdata/01-minimal.program.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// sha256Digest formats the `sha256:<hex>` digest string §6.2 requires.
func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canvasEnvelope builds the `zabcanvas.resolved-scene.v1` slice
// decodeAndVerifyProgram consumes: the program bytes base64-encoded
// alongside their own digest, self-consistent by construction.
func canvasEnvelope(program []byte) (json.RawMessage, string) {
	digest := sha256Digest(program)
	body, _ := json.Marshal(resolvedSceneEnvelope{
		BlueProgram:       base64.StdEncoding.EncodeToString(program),
		BlueProgramDigest: digest,
	})
	return body, digest
}

func signedRef(t *testing.T, priv ed25519.PrivateKey, kid string, _ attestation.Action, now time.Time, blueProgramDigest string) string {
	t.Helper()
	header := map[string]any{"alg": "EdDSA", "kid": kid, "typ": "zabcanvas-resolved-scene-ref+jws"}
	payload := map[string]any{
		"schema_version":           "1",
		"ref_id":                   "ref-1",
		"attestation_id":           "att-1",
		"issuer":                   "https://zabcanvas.internal",
		"audience":                 "orion",
		"subject":                  "operator-1",
		"owner_id":                 "owner-1",
		"tenant_id":                "tenant-1",
		"stream_id":                "stream-1",
		"allowed_actions":          []string{"prepare-preview", "take-on-air"},
		"scene_id":                 "scene-1",
		"revision_id":              "rev-1",
		"scene_digest":             "sha256:" + strings.Repeat("a", 64),
		"artifact_set_digest":      "sha256:" + strings.Repeat("b", 64),
		"blue_program_digest":      blueProgramDigest,
		"readiness_attestation_id": "ready-1",
		"readiness_digest":         "sha256:" + strings.Repeat("d", 64),
		"readiness_expires_at":     now.Add(time.Hour).Unix(),
		"issued_at":                now.Unix(),
		"not_before":               now.Add(-time.Minute).Unix(),
		"expires_at":               now.Add(time.Hour).Unix(),
		"canvas_locator":           "scenes/scene-1/revisions/rev-1",
	}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(payload)
	h64 := base64.RawURLEncoding.EncodeToString(hb)
	p64 := base64.RawURLEncoding.EncodeToString(pb)
	sig := ed25519.Sign(priv, []byte(h64+"."+p64))
	s64 := base64.RawURLEncoding.EncodeToString(sig)
	return h64 + "." + p64 + "." + s64
}

func TestPostSceneIntent_PreparePreview_Success(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, digest)

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
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
	if resp.Status != "prepared" || resp.SceneID != "scene-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestPostSceneIntent_RejectsMissingTicket(t *testing.T) {
	deps := SceneIntentDeps{Host: bluehost.NewHost()}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")

	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestPostSceneIntent_RejectsBadAttestation(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil) // key not in trust set
	otherPub, _, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "sha256:"+strings.Repeat("c", 64))

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": otherPub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{},
		Host:          bluehost.NewHost(),
	}
	body, _ := json.Marshal(sceneIntentRequest{
		IntentID: "intent-1", StreamID: "stream-1", Action: string(attestation.ActionPreparePreview), ResolvedSceneRef: ref,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "opaque-ticket")

	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPostSceneIntent_WorkloadRefusalPropagates(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "sha256:"+strings.Repeat("c", 64))

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{admitErr: &workload.Error{Code: workload.CodeAuthContextExpired}},
		Host:          bluehost.NewHost(),
	}
	body, _ := json.Marshal(sceneIntentRequest{
		IntentID: "intent-1", StreamID: "stream-1", Action: string(attestation.ActionPreparePreview), ResolvedSceneRef: ref,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "opaque-ticket")

	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPostSceneIntent_RejectsArtifactDigestMismatch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	_, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, digest)

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: json.RawMessage(`{"blue_program":"` + base64.StdEncoding.EncodeToString([]byte("tampered-bytes")) + `","blue_program_digest":"` + digest + `"}`)},
		Host:          bluehost.NewHost(),
	}
	body, _ := json.Marshal(sceneIntentRequest{
		IntentID: "intent-1", StreamID: "stream-1", Action: string(attestation.ActionPreparePreview), ResolvedSceneRef: ref,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "opaque-ticket")

	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 (digest mismatch), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPostSceneIntent_RequiresOperatorRole(t *testing.T) {
	deps := SceneIntentDeps{Host: bluehost.NewHost()}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader([]byte(`{}`)))
	// No X-Authenticated-* headers ⇒ anonymous.
	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (operator gate), got %d", rec.Code)
	}
}
