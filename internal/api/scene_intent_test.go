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
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/workload"
)

type fakeWorkload struct {
	admitErr   error
	mintErr    error
	fetchErr   error
	body       json.RawMessage
	admitCalls int
}

func (f *fakeWorkload) AdmitAuthContext(_ context.Context, _ string) (*workload.AuthContextAdmission, error) {
	f.admitCalls++
	if f.admitErr != nil {
		return nil, f.admitErr
	}
	return &workload.AuthContextAdmission{AdmissionID: "adm-1", IntentID: "intent-1"}, nil
}

func (f *fakeWorkload) MintDelegation(_ context.Context, _ *workload.AuthContextAdmission) (*workload.Delegation, error) {
	if f.mintErr != nil {
		return nil, f.mintErr
	}
	return &workload.Delegation{JTI: "jti-1"}, nil
}

func (f *fakeWorkload) FetchCanvas(_ context.Context, _ string) (*workload.CanvasArtifact, error) {
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

type recordingMirror struct {
	mu        sync.Mutex
	forwarded int
}

func (m *recordingMirror) Forward(any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forwarded++
}

func (m *recordingMirror) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.forwarded
}

// TestPostSceneIntent_BridgeFullLifecycle proves the bridge Conduit asked
// for: PreparePreview starts a bridge forwarding onto the resolved
// mirror, and releaseSlot stops it cleanly (no further forwards, no
// panic on the subsequent bluehost.Release).
func TestPostSceneIntent_BridgeFullLifecycle(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, digest)

	mirror := &recordingMirror{}
	deps := SceneIntentDeps{
		Trust:              attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix:      "scenes/",
		OwnerID:            "owner-1",
		TenantID:           "tenant-1",
		Workload:           &fakeWorkload{body: envelope},
		Host:               bluehost.NewHost(),
		MirrorFor:          func(string) runtime.SceneMirror { return mirror },
		Bridges:            bluewire.NewRegistry(),
		ProjectionInterval: 5 * time.Millisecond,
	}

	body, _ := json.Marshal(sceneIntentRequest{
		IntentID: "intent-1", StreamID: "stream-1", Target: "preview",
		Action: string(attestation.ActionPreparePreview), ResolvedSceneRef: ref,
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

	if !deps.Bridges.Running(bluehost.SlotPreview) {
		t.Fatal("expected the bridge to be running after a successful prepare-preview")
	}

	// The minimal fixture's on-start entrypoint produces no leaf output,
	// so Bridge.StepOnce's empty-projection no-op (mirroring the legacy
	// zero-patch-delta drop) means mirror.Forward is never actually
	// called here — this lifecycle test asserts the START/STOP wiring
	// itself (Running before, not Running after), not payload delivery,
	// which internal/bluewire's own tests already cover with a
	// non-empty-output fake.
	time.Sleep(30 * time.Millisecond)

	if err := releaseSlot(deps, bluehost.SlotPreview, "test-release"); err != nil {
		t.Fatalf("releaseSlot: %v", err)
	}
	if deps.Bridges.Running(bluehost.SlotPreview) {
		t.Fatal("expected the bridge to be stopped after releaseSlot")
	}

	countAtRelease := mirror.count()
	time.Sleep(30 * time.Millisecond)
	if mirror.count() != countAtRelease {
		t.Fatalf("expected no further forwards after release, count went from %d to %d", countAtRelease, mirror.count())
	}
}

func TestPostSceneIntent_IdempotentReplaySkipsReExecution(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, digest)

	wl := &fakeWorkload{body: envelope}
	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      wl,
		Host:          bluehost.NewHost(),
		Idempotency:   NewIdempotencyCache(),
	}

	body, _ := json.Marshal(sceneIntentRequest{
		IntentID: "intent-1", IdempotencyKey: "idem-1", StreamID: "stream-1",
		Action: string(attestation.ActionPreparePreview), ResolvedSceneRef: ref,
	})

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
		req.Header.Set("X-Authenticated-User", "operator-1")
		req.Header.Set("X-Authenticated-Role", "operator")
		req.Header.Set(authContextHeader, "opaque-ticket")
		rec := httptest.NewRecorder()
		postSceneIntent(deps)(rec, req)
		return rec
	}

	first := send()
	if first.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d: %s", first.Code, first.Body.String())
	}
	if wl.admitCalls != 1 {
		t.Fatalf("expected 1 workload admission on first request, got %d", wl.admitCalls)
	}

	second := send()
	if second.Code != http.StatusOK {
		t.Fatalf("second request: expected 200, got %d: %s", second.Code, second.Body.String())
	}
	if wl.admitCalls != 1 {
		t.Fatalf("expected the replay to skip re-execution (still 1 workload admission), got %d", wl.admitCalls)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("expected the replay to return the identical cached result, got %q vs %q", first.Body.String(), second.Body.String())
	}
}

func TestPostSceneIntent_NoIdempotencyKeyNeverDedupes(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, digest)

	wl := &fakeWorkload{body: envelope}
	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      wl,
		Host:          bluehost.NewHost(),
		Idempotency:   NewIdempotencyCache(),
	}

	body, _ := json.Marshal(sceneIntentRequest{
		IntentID: "intent-1", StreamID: "stream-1", // no IdempotencyKey
		Action: string(attestation.ActionPreparePreview), ResolvedSceneRef: ref,
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
		req.Header.Set("X-Authenticated-User", "operator-1")
		req.Header.Set("X-Authenticated-Role", "operator")
		req.Header.Set(authContextHeader, "opaque-ticket")
		rec := httptest.NewRecorder()
		postSceneIntent(deps)(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if wl.admitCalls != 2 {
		t.Fatalf("expected every request without an idempotency_key to re-execute, got %d admissions", wl.admitCalls)
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

func TestGetHostStatus_EmptyHost(t *testing.T) {
	deps := SceneIntentDeps{Host: bluehost.NewHost()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/host/status", nil)
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")

	rec := httptest.NewRecorder()
	getHostStatus(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp hostStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Preview.Loaded || resp.OnAir.Loaded {
		t.Fatalf("expected an empty host to report nothing loaded, got %+v", resp)
	}
}

func TestGetHostStatus_ReflectsPreparedSlot(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "instance-1", "sha256:abc", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	deps := SceneIntentDeps{Host: host}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/host/status", nil)
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")

	rec := httptest.NewRecorder()
	getHostStatus(deps)(rec, req)
	var resp hostStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Preview.Loaded || resp.Preview.SceneDigest != "sha256:abc" {
		t.Fatalf("expected preview loaded with sha256:abc, got %+v", resp.Preview)
	}
	if resp.OnAir.Loaded {
		t.Fatalf("expected on_air empty, got %+v", resp.OnAir)
	}
}

func TestGetHostStatus_RequiresOperatorRole(t *testing.T) {
	deps := SceneIntentDeps{Host: bluehost.NewHost()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/host/status", nil)
	rec := httptest.NewRecorder()
	getHostStatus(deps)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestDecodeAndVerifyBundle_ValidBundle(t *testing.T) {
	raw := []byte(`{"root":{}}`)
	digest := sha256Digest(raw)
	body, _ := json.Marshal(resolvedSceneEnvelope{
		LSMLBundle:       base64.StdEncoding.EncodeToString(raw),
		LSMLBundleDigest: digest,
	})
	got, err := decodeAndVerifyBundle(body)
	if err != nil {
		t.Fatalf("decodeAndVerifyBundle: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("expected %q, got %q", raw, got)
	}
}

func TestDecodeAndVerifyBundle_NoBundleIsNilNil(t *testing.T) {
	body, _ := json.Marshal(resolvedSceneEnvelope{})
	got, err := decodeAndVerifyBundle(body)
	if err != nil || got != nil {
		t.Fatalf("expected (nil, nil) for an envelope with no bundle, got (%v, %v)", got, err)
	}
}

func TestDecodeAndVerifyBundle_DigestMismatchRejected(t *testing.T) {
	body, _ := json.Marshal(resolvedSceneEnvelope{
		LSMLBundle:       base64.StdEncoding.EncodeToString([]byte(`{"root":{}}`)),
		LSMLBundleDigest: "sha256:" + strings.Repeat("f", 64),
	})
	if _, err := decodeAndVerifyBundle(body); err == nil {
		t.Fatal("expected digest mismatch to be rejected")
	}
}

// TestDecodeAndVerifyBundle_MissingDigestRejected — Bastion C4 (PR #346):
// a bundle present without its digest must fail closed, never ride
// unverified through to GET /host/render-bundle.
func TestDecodeAndVerifyBundle_MissingDigestRejected(t *testing.T) {
	body, _ := json.Marshal(resolvedSceneEnvelope{
		LSMLBundle: base64.StdEncoding.EncodeToString([]byte(`{"root":{}}`)),
	})
	if _, err := decodeAndVerifyBundle(body); err == nil {
		t.Fatal("expected missing lsml_bundle_digest to be rejected (fail-closed)")
	}
}

func TestGetHostRenderBundle_ServesPreparedBundle(t *testing.T) {
	host := bluehost.NewHost()
	program := minimalProgram(t)
	if err := host.Prepare(bluehost.SlotPreview, "instance-1", "sha256:abc", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	host.SetBundle(bluehost.SlotPreview, []byte(`{"root":{}}`))
	deps := SceneIntentDeps{Host: host}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/host/render-bundle", nil)
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	rec := httptest.NewRecorder()
	getHostRenderBundle(deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"root":{}}` {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Fatal("expected an immutable cache header")
	}
	if rec.Header().Get("ETag") != `"sha256:abc"` {
		t.Fatalf("unexpected ETag: %q", rec.Header().Get("ETag"))
	}
}

func TestGetHostRenderBundle_MissingSlotIs404(t *testing.T) {
	deps := SceneIntentDeps{Host: bluehost.NewHost()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/host/render-bundle", nil)
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	rec := httptest.NewRecorder()
	getHostRenderBundle(deps)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
