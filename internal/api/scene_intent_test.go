package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/workload"
)

// fakeIdempotencyMetrics records IdempotencyCache evictions for assertion.
type fakeIdempotencyMetrics struct {
	mu       sync.Mutex
	ttl      int
	capacity int
}

func (f *fakeIdempotencyMetrics) IdempotencyEvicted(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch reason {
	case "ttl":
		f.ttl++
	case "capacity":
		f.capacity++
	}
}

// TestIdempotencyCache_TTLWindowExpiresAndIsCounted is the nominal
// replay/dedup-window proof (ADR-BLUE-012 §6.4/§12, B3-R6-OPS-ORION): a
// tuple stored at t0 is served from cache within the TTL, and treated as a
// fresh miss (not returned stale) once the TTL has elapsed — the eviction
// is counted with reason "ttl".
func TestIdempotencyCache_TTLWindowExpiresAndIsCounted(t *testing.T) {
	metrics := &fakeIdempotencyMetrics{}
	c := NewIdempotencyCacheWithLimits(10*time.Second, 100, metrics)
	now := time.Unix(1_700_000_000, 0)
	c.setNowForTest(func() time.Time { return now })

	c.store("k1", sceneIntentResponse{})
	if _, ok := c.lookup("k1"); !ok {
		t.Fatal("expected a hit immediately after store, within the TTL window")
	}

	// Still within the window (9s < 10s TTL): still a hit.
	now = now.Add(9 * time.Second)
	if _, ok := c.lookup("k1"); !ok {
		t.Fatal("expected a hit at 9s into a 10s TTL window")
	}

	// Past the window: a miss, counted as a "ttl" eviction — never a stale
	// replay returned past its window.
	now = now.Add(2 * time.Second) // t0+11s > 10s TTL
	if _, ok := c.lookup("k1"); ok {
		t.Fatal("expected a miss past the TTL window (replay window must expire, not persist forever)")
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if metrics.ttl != 1 {
		t.Fatalf("expected 1 counted ttl eviction, got %d", metrics.ttl)
	}
}

// TestIdempotencyCache_SaturationBoundsMemoryAndIsCounted is the
// saturation proof for the cache's OTHER axis (ADR-BLUE-012 §12/B8 —
// "surcharge non bornée" is the principal named risk): storing far more
// distinct tuples than maxEntries never grows the map past the cap, and
// the eviction that keeps it bounded is counted with reason "capacity".
// Fail-closed behavior: an evicted tuple's replay is processed FRESH
// (a miss), never denied outright — bounding memory never blocks traffic.
func TestIdempotencyCache_SaturationBoundsMemoryAndIsCounted(t *testing.T) {
	metrics := &fakeIdempotencyMetrics{}
	const maxEntries = 8
	// A TTL long enough that capacity, not staleness, is what triggers
	// eviction — isolates the axis under test from TestIdempotencyCache_
	// TTLWindowExpiresAndIsCounted above.
	c := NewIdempotencyCacheWithLimits(time.Hour, maxEntries, metrics)
	now := time.Unix(1_700_000_000, 0)
	c.setNowForTest(func() time.Time { return now })

	const totalKeys = 64 // far beyond maxEntries: guarantees saturation
	for i := 0; i < totalKeys; i++ {
		now = now.Add(time.Millisecond) // strict insertion order for the oldest-evict check
		c.store(fmt.Sprintf("k%d", i), sceneIntentResponse{})
	}

	c.mu.Lock()
	size := len(c.cache)
	c.mu.Unlock()
	if size > maxEntries {
		t.Fatalf("cache grew to %d entries, want <= %d (maxEntries bound violated — unbounded memory)", size, maxEntries)
	}

	metrics.mu.Lock()
	capacityEvictions := metrics.capacity
	metrics.mu.Unlock()
	if capacityEvictions == 0 {
		t.Fatalf("expected counted capacity evictions storing %d keys into a %d-entry cache, got 0", totalKeys, maxEntries)
	}

	// Fail-closed-but-not-fail-shut: an evicted (oldest) tuple is a MISS on
	// replay, never an error — the caller re-executes fresh.
	if _, ok := c.lookup("k0"); ok {
		t.Fatal("expected the oldest key to have been evicted under capacity pressure")
	}
	// The most recently stored key must still be resident.
	if _, ok := c.lookup(fmt.Sprintf("k%d", totalKeys-1)); !ok {
		t.Fatal("expected the most recently stored key to survive capacity eviction")
	}
}

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
	return canvasEnvelopeWithBundle(program, nil)
}

func canvasEnvelopeWithBundle(program, bundle []byte) (json.RawMessage, string) {
	digest := sha256Digest(program)
	envelope := resolvedSceneEnvelope{
		BlueProgram:       base64.StdEncoding.EncodeToString(program),
		BlueProgramDigest: digest,
	}
	if bundle != nil {
		envelope.LSMLBundle = base64.StdEncoding.EncodeToString(bundle)
		envelope.LSMLBundleDigest = sha256Digest(bundle)
	}
	body, _ := json.Marshal(envelope)
	return body, digest
}

func signedRef(t *testing.T, priv ed25519.PrivateKey, kid string, _ attestation.Action, now time.Time, sceneID, blueProgramDigest string) string {
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
		"scene_id":                 sceneID,
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
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

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

// TestPostSceneIntent_PreparePreview_SameSceneSameDigestIsIdempotent is the
// "must still short-circuit" direction of the ADR-BLUE-012 §4.4 slot-identity
// fix: a genuine repeat — same scene_id, same digest — must keep succeeding.
// Host.Prepare unconditionally refuses (ErrAlreadyLoaded) any second call on
// an occupied slot BEFORE it ever touches the runtime (host.go's existence
// check is the first statement in Prepare, ahead of runtime.Load/Start) — so
// a second response of "prepared" here is only reachable via the caller's
// Host.Serving short-circuit swallowing that refusal, which is itself proof
// no re-Load happened for the repeat.
func TestPostSceneIntent_PreparePreview_SameSceneSameDigestIsIdempotent(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
	}

	send := func() *httptest.ResponseRecorder {
		ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)
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
		return rec
	}

	if first := send(); first.Code != http.StatusOK {
		t.Fatalf("first prepare: expected 200, got %d: %s", first.Code, first.Body.String())
	}

	second := send()
	if second.Code != http.StatusOK {
		t.Fatalf("idempotent re-prepare (same scene, same digest): expected 200, got %d: %s", second.Code, second.Body.String())
	}
	var resp sceneIntentResponse
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "prepared" {
		t.Fatalf("expected the idempotent repeat to still report \"prepared\" (no tear-down for a no-op push), got %+v", resp)
	}
	// signedRef's scene_digest claim (distinct from blueProgramDigest/digest
	// above) is the fixed "sha256:aaa...a" every call in this file carries.
	sceneDigest := "sha256:" + strings.Repeat("a", 64)
	if !deps.Host.Serving(bluehost.SlotPreview, "scene-1", sceneDigest) {
		t.Fatal("expected the preview slot to still be serving (scene-1, scene_digest) after the idempotent repeat")
	}
}

// TestPostSceneIntent_PreparePreview_DifferentSceneSameDigestIsRejected is
// the direction that actually matters: two DIFFERENT scenes sharing a
// scene_digest — plausible today (pre-C3, scene_digest is version-derived,
// not content-bound) and still possible once ZabCanvas PR#340 lands (two
// distinct scenes can compile to byte-identical programs) — must never let
// the second scene's prepare-preview silently reuse the first scene's
// already-running instance. Before the fix, Host.Digest(slot) ==
// claims.SceneDigest alone read this as idempotent and dropped scene B's
// program on the floor; scene A's instance (and its accumulated state) kept
// serving under scene B's intent with a 200 response. Now sceneID must also
// match, so scene B is refused with a named error instead.
func TestPostSceneIntent_PreparePreview_DifferentSceneSameDigestIsRejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
	}

	send := func(sceneID, intentID string) *httptest.ResponseRecorder {
		// signedRef's scene_digest claim is a fixed "sha256:aaa...a" for
		// every call regardless of sceneID/blueProgramDigest — exactly the
		// pre-C3 shape this test targets: two scenes sharing one digest.
		ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, sceneID, digest)
		body, _ := json.Marshal(sceneIntentRequest{
			IntentID:         intentID,
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
		return rec
	}

	sceneA := send("scene-A", "intent-a")
	if sceneA.Code != http.StatusOK {
		t.Fatalf("scene A prepare: expected 200, got %d: %s", sceneA.Code, sceneA.Body.String())
	}

	sceneB := send("scene-B", "intent-b")
	if sceneB.Code != http.StatusInternalServerError {
		t.Fatalf("expected scene B's prepare-preview to be rejected (500 HOST_PREPARE_FAILED) instead of silently reusing scene A's instance, got %d: %s", sceneB.Code, sceneB.Body.String())
	}
	var resp sceneIntentResponse
	if err := json.Unmarshal(sceneB.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "failed" || resp.Reason != "HOST_PREPARE_FAILED" {
		t.Fatalf("expected a named HOST_PREPARE_FAILED rejection, not a silent ok, got %+v", resp)
	}
	// Scene A's intent must be untouched by scene B's refused one.
	// signedRef's scene_digest claim (distinct from blueProgramDigest/digest
	// above) is the fixed "sha256:aaa...a" both scene A and scene B carry —
	// exactly the collision this test forces.
	sceneDigest := "sha256:" + strings.Repeat("a", 64)
	if !deps.Host.Serving(bluehost.SlotPreview, "scene-A", sceneDigest) {
		t.Fatal("expected scene A's instance to remain the preview slot's occupant after scene B's rejected intent")
	}
}

func TestPostSceneIntent_TakeOnAirOwnsBundleAndBridgeOnAir(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	bundle := []byte(`{"scene":"on-air"}`)
	envelope, digest := canvasEnvelopeWithBundle(program, bundle)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)
	host := bluehost.NewHost()
	bridges := bluewire.NewRegistry()
	t.Cleanup(func() {
		bridges.StopAll()
		_ = host.Release(bluehost.SlotPreview, "test-cleanup")
		_ = host.Release(bluehost.SlotOnAir, "test-cleanup")
	})
	deps := SceneIntentDeps{
		Trust:              attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix:      "scenes/",
		OwnerID:            "owner-1",
		TenantID:           "tenant-1",
		Workload:           &fakeWorkload{body: envelope},
		Host:               host,
		MirrorFor:          func(string, string, bluehost.Slot, []byte) runtime.SceneMirror { return &recordingMirror{} },
		Bridges:            bridges,
		ProjectionInterval: time.Hour,
	}

	send := func(action attestation.Action, target string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(sceneIntentRequest{
			IntentID:         "intent-" + string(action),
			StreamID:         "stream-1",
			Target:           target,
			Action:           string(action),
			ResolvedSceneRef: ref,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
		req.Header.Set("X-Authenticated-User", "operator-1")
		req.Header.Set("X-Authenticated-Role", "operator")
		req.Header.Set(authContextHeader, "opaque-ticket")
		rec := httptest.NewRecorder()
		postSceneIntent(deps)(rec, req)
		return rec
	}

	if rec := send(attestation.ActionPreparePreview, "preview"); rec.Code != http.StatusOK {
		t.Fatalf("prepare-preview: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := host.Bundle(bluehost.SlotPreview); string(got) != string(bundle) {
		t.Fatalf("prepare-preview bundle attached to wrong value: %q", got)
	}
	if host.Bundle(bluehost.SlotOnAir) != nil {
		t.Fatal("prepare-preview must not attach a bundle to on-air")
	}
	if !bridges.Running(bluehost.SlotPreview) {
		t.Fatal("prepare-preview should run the preview bridge")
	}

	if rec := send(attestation.ActionTakeOnAir, "on-air"); rec.Code != http.StatusOK {
		t.Fatalf("take-on-air: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := host.Bundle(bluehost.SlotOnAir); string(got) != string(bundle) {
		t.Fatalf("take-on-air bundle was not attached to on-air: %q", got)
	}
	if !bridges.Running(bluehost.SlotOnAir) {
		t.Fatal("take-on-air should run the on-air bridge")
	}
}

func TestPostSceneIntent_TakeOnAirFailurePreservesCommittedGeneration(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	host := bluehost.NewHost()
	if err := host.Take("old-instance", "scene-1", "sha256:old", program, nil, nil, nil); err != nil {
		t.Fatalf("seed Host.Take: %v", err)
	}
	oldBundle := []byte(`{"scene":"old"}`)
	host.SetBundle(bluehost.SlotOnAir, oldBundle)
	bridges := bluewire.NewRegistry()
	bridges.Start(bluehost.SlotOnAir, bluewire.NewBridge(host, bluehost.SlotOnAir, &recordingMirror{}, "scene-1", "sha256:old", "old-instance", blueproject.TargetProgram, "rev-1", "intent-old"), time.Hour, nil)
	t.Cleanup(func() {
		bridges.StopAll()
		_ = host.Release(bluehost.SlotOnAir, "test-cleanup")
	})

	invalidProgram := []byte(`{"not":"a blue program"}`)
	envelope, digest := canvasEnvelope(invalidProgram)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionTakeOnAir, now, "scene-1", digest)
	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          host,
		Bridges:       bridges,
	}
	body, _ := json.Marshal(sceneIntentRequest{
		IntentID:         "intent-failed-take",
		StreamID:         "stream-1",
		Target:           "on-air",
		Action:           string(attestation.ActionTakeOnAir),
		ResolvedSceneRef: ref,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "opaque-ticket")
	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected failed take to return 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := host.Digest(bluehost.SlotOnAir); got != "sha256:old" {
		t.Fatalf("failed pre-commit take changed on-air digest to %q", got)
	}
	if got := host.Bundle(bluehost.SlotOnAir); string(got) != string(oldBundle) {
		t.Fatalf("failed pre-commit take changed on-air bundle to %q", got)
	}
	if !bridges.Running(bluehost.SlotOnAir) {
		t.Fatal("failed pre-commit take must preserve the committed on-air bridge")
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
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", "sha256:"+strings.Repeat("c", 64))

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
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", "sha256:"+strings.Repeat("c", 64))

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
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

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
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

	mirror := &recordingMirror{}
	deps := SceneIntentDeps{
		Trust:              attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix:      "scenes/",
		OwnerID:            "owner-1",
		TenantID:           "tenant-1",
		Workload:           &fakeWorkload{body: envelope},
		Host:               bluehost.NewHost(),
		MirrorFor:          func(string, string, bluehost.Slot, []byte) runtime.SceneMirror { return mirror },
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

// TestPostSceneIntent_MirrorForReceivesRealSceneDigest is the sceneVersion
// coherence half of ORION-TAKE-SLOT-IDENTITY (Blue#345): startBridge must
// pass claims.SceneDigest — the SAME digest Prepare/Take committed on the
// slot — as MirrorFor's sceneVersion argument, never a hardcoded "".
// Before this fix, cmd/orion/main.go's MirrorFor closure hardcoded "" for
// every call, so whatever value the LSDP kit told Solar its scene_version
// was, Solar could never echo back a ?v= that resolveHostBundle (#401)
// would accept — Take's own sceneID fix alone is not sufficient for a real
// client to ever reach the resolver with a matching pair.
func TestPostSceneIntent_MirrorForReceivesRealSceneDigest(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

	var gotSceneID, gotSceneVersion string
	var calls int
	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
		MirrorFor: func(sceneID, sceneVersion string, _ bluehost.Slot, _ []byte) runtime.SceneMirror {
			gotSceneID = sceneID
			gotSceneVersion = sceneVersion
			calls++
			return &recordingMirror{}
		},
		Bridges:            bluewire.NewRegistry(),
		ProjectionInterval: time.Hour,
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

	if calls != 1 {
		t.Fatalf("expected MirrorFor called exactly once, got %d", calls)
	}
	if gotSceneID != "scene-1" {
		t.Fatalf("expected sceneID %q, got %q", "scene-1", gotSceneID)
	}
	if gotSceneVersion == "" {
		t.Fatal("MirrorFor received an empty sceneVersion — the #398-class defect: a client can never learn a ?v= that resolveHostBundle would accept")
	}
	// scene_digest (claims.SceneDigest) is the fixture's fixed "aaa..."
	// literal in signedRef — distinct from blue_program_digest (the
	// canvasEnvelope-returned `digest` used for the program cross-check).
	// It is the SAME value deps.Host.Prepare/Take are already called with —
	// the value host.Digest(slot) will hold, so it is what a matching ?v=
	// must equal.
	wantSceneVersion := "sha256:" + strings.Repeat("a", 64)
	if gotSceneVersion != wantSceneVersion {
		t.Fatalf("expected sceneVersion to equal claims.SceneDigest %q, got %q", wantSceneVersion, gotSceneVersion)
	}
}

func TestPostSceneIntent_IdempotentReplaySkipsReExecution(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

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
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

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
	if err := host.Prepare(bluehost.SlotPreview, "instance-1", "scene-1", "sha256:abc", program, nil, nil, nil); err != nil {
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

// TestGetHostStatus_ReflectsLastForwardedProjection proves the priority-3
// extension end to end: once a slot's bridge has forwarded at least one
// projection, GET /api/v1/host/status surfaces the SAME correlation_id/
// render_revision that rode the LSDP delta (bluewire.Bridge.LastForwarded),
// as a stateless polling convenience — never anything Orion waited on to
// answer this request (Refs B3-R6-16-ORION-PGM).
func TestGetHostStatus_ReflectsLastForwardedProjection(t *testing.T) {
	host := bluehost.NewHost()
	// chatDrivenProgram (not minimalProgram, which is display-only with no
	// entrypoint output) is the same fixture
	// TestChatDrivenScene_InjectionOrderingIdempotenceProjection already
	// proves emits a real baseline projection on the very first tick, with
	// zero events injected — exactly the deterministic, non-empty forward
	// this test needs.
	program := chatOverlayProgramFixture(t)
	if err := host.Prepare(bluehost.SlotOnAir, "instance-1", "scene-1", "sha256:abc", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = host.Release(bluehost.SlotOnAir, "test-cleanup") })

	bridge := bluewire.NewBridge(host, bluehost.SlotOnAir, &recordingMirror{}, "scene-1", "sha256:abc", "instance-1", blueproject.TargetProgram, "rev-1", "corr-1")
	if err := bridge.TickOnce(0.1); err != nil {
		t.Fatalf("bridge.TickOnce: %v", err)
	}

	bridges := bluewire.NewRegistry()
	bridges.Start(bluehost.SlotOnAir, bridge, time.Hour, nil)
	t.Cleanup(bridges.StopAll)

	deps := SceneIntentDeps{Host: host, Bridges: bridges}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/host/status", nil)
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")

	rec := httptest.NewRecorder()
	getHostStatus(deps)(rec, req)
	var resp hostStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OnAir.Projection == nil {
		t.Fatalf("expected on_air projection to be populated, got %+v", resp.OnAir)
	}
	if resp.OnAir.Projection.CorrelationID != "corr-1" || resp.OnAir.Projection.RenderRevision != "rev-1" {
		t.Fatalf("unexpected projection identity: %+v", resp.OnAir.Projection)
	}
	if resp.Preview.Projection != nil {
		t.Fatalf("expected preview projection to stay nil (no bridge running there), got %+v", resp.Preview.Projection)
	}
}

// TestGetHostStatus_NilBridgesOmitsProjection proves the field degrades to
// absent (never a zero-value/invented identity) when deps.Bridges is nil —
// the same dark-by-default posture MirrorFor/Idempotency already have
// (scene_intent.go's SceneIntentDeps doc comments).
func TestGetHostStatus_NilBridgesOmitsProjection(t *testing.T) {
	deps := SceneIntentDeps{Host: bluehost.NewHost()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/host/status", nil)
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")

	rec := httptest.NewRecorder()
	getHostStatus(deps)(rec, req)
	var resp hostStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Preview.Projection != nil || resp.OnAir.Projection != nil {
		t.Fatalf("expected no projection with a nil Bridges registry, got %+v", resp)
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
	if err := host.Prepare(bluehost.SlotPreview, "instance-1", "scene-1", "sha256:abc", program, nil, nil, nil); err != nil {
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
