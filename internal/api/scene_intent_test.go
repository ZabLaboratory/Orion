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
	"github.com/ZabLaboratory/Orion/internal/canonical"
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
	mintErr  error
	fetchErr error
	body     json.RawMessage

	mintCalls  int
	mintTicket string
	mintIntent json.RawMessage

	fetchCalls      int
	fetchDelegation *workload.Delegation
	fetchTicket     string
	fetchIntent     json.RawMessage
}

func (f *fakeWorkload) MintDelegation(_ context.Context, ticket string, intent json.RawMessage) (*workload.Delegation, error) {
	f.mintCalls++
	f.mintTicket = ticket
	f.mintIntent = append(json.RawMessage(nil), intent...)
	if f.mintErr != nil {
		return nil, f.mintErr
	}
	return &workload.Delegation{
		JTI: "jti-1", AccessToken: "opaque-delegation", TokenType: "bearer", Status: "issued",
		GateRequestID: "3f2c8f0e-2f65-4e11-8a63-0123456789ab." + strings.Repeat("d", 64),
	}, nil
}

func (f *fakeWorkload) FetchCanvas(_ context.Context, delegation *workload.Delegation, ticket string, intent json.RawMessage) (*workload.CanvasArtifact, error) {
	f.fetchCalls++
	f.fetchDelegation = delegation
	f.fetchTicket = ticket
	f.fetchIntent = append(json.RawMessage(nil), intent...)
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
	decoder := json.NewDecoder(bytes.NewReader(program))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		panic(err)
	}
	claimedDigest, ok := document["program_digest"].(string)
	if !ok || claimedDigest == "" {
		panic("test program is missing program_digest")
	}
	delete(document, "program_digest")
	digest, err := canonical.Digest(document)
	if err != nil {
		panic(err)
	}
	if digest != claimedDigest {
		panic(fmt.Sprintf("test program digest mismatch: computed=%s claimed=%s", digest, claimedDigest))
	}
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
		"kid":                      kid,
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

// TestPostSceneIntent_PreparePreview_DifferentSceneSameDigestReplaces proves
// that two DIFFERENT scenes sharing a scene_digest can switch the preview
// slot. The same digest is plausible today (pre-C3, scene_digest is
// version-derived, not content-bound) and still possible once ZabCanvas PR#340
// lands (two distinct scenes can compile to byte-identical programs). The
// second scene must replace the first instance, never reuse its state.
func TestPostSceneIntent_PreparePreview_DifferentSceneSameDigestReplaces(t *testing.T) {
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
	if sceneB.Code != http.StatusOK {
		t.Fatalf("scene B prepare replacement: expected 200, got %d: %s", sceneB.Code, sceneB.Body.String())
	}
	var resp sceneIntentResponse
	if err := json.Unmarshal(sceneB.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "prepared" || resp.SceneID != "scene-B" {
		t.Fatalf("expected scene B to be prepared, got %+v", resp)
	}
	// signedRef's scene_digest claim (distinct from blueProgramDigest/digest
	// above) is the fixed "sha256:aaa...a" both scene A and scene B carry —
	// exactly the collision this test forces.
	sceneDigest := "sha256:" + strings.Repeat("a", 64)
	if !deps.Host.Serving(bluehost.SlotPreview, "scene-B", sceneDigest) {
		t.Fatal("expected scene B to occupy the preview slot after replacement")
	}
	if deps.Host.Serving(bluehost.SlotPreview, "scene-A", sceneDigest) {
		t.Fatal("expected scene A to leave the preview slot after replacement")
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

	invalidDocument := map[string]any{"not": "a blue program"}
	invalidDigest, err := canonical.Digest(invalidDocument)
	if err != nil {
		t.Fatalf("compute invalid program digest: %v", err)
	}
	invalidDocument["program_digest"] = invalidDigest
	invalidProgram, err := json.Marshal(invalidDocument)
	if err != nil {
		t.Fatalf("marshal invalid program: %v", err)
	}
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
		Workload:      &fakeWorkload{mintErr: &workload.Error{Code: workload.CodeAuthContextExpired}},
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
	// The reason must be the BARE §4.7 code — Prism's failure vocabulary
	// matches on it exactly; the old err.Error() spelling ("workload:
	// CODE (http n)") collapsed every typed refusal into
	// UNKNOWN_RESPONSE on the operator's screen.
	var resp sceneIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Reason != string(workload.CodeAuthContextExpired) {
		t.Fatalf("expected bare code %q, got %q", workload.CodeAuthContextExpired, resp.Reason)
	}
}

// TestPostSceneIntent_RelaysTicketAndRawIntentVerbatim is the M3
// point of vigilance: the intent must travel BYTE-FOR-BYTE from the
// request body into MintDelegation — Gate binds its values (deadline
// among them) into the ticket, so any reconstruction or
// re-serialization on Orion's side is an AUTH_CONTEXT_MISMATCH. The
// posted body carries fields Orion's own sceneIntentRequest does not
// model (sequence, issued_at, deadline, correlation_id) plus a
// max-safe-integer deadline; byte equality at the portal proves none
// of it was dropped or mutated.
func TestPostSceneIntent_RelaysTicketAndRawIntentVerbatim(t *testing.T) {
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
	}

	body, _ := json.Marshal(map[string]any{
		"schema_version":     "orion.scene-intent.v1",
		"intent_id":          "intent-1",
		"idempotency_key":    "idem-1",
		"sequence":           7,
		"stream_id":          "stream-1",
		"target":             "preview",
		"action":             string(attestation.ActionPreparePreview),
		"resolved_scene_ref": ref,
		"issued_at":          1800000000,
		"deadline":           json.Number("9007199254740991"),
		"correlation_id":     "corr-1",
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
	if wl.mintCalls != 1 {
		t.Fatalf("expected exactly one mint, got %d", wl.mintCalls)
	}
	if wl.mintTicket != "opaque-ticket" {
		t.Fatalf("mint must receive the relayed ticket, got %q", wl.mintTicket)
	}
	if !bytes.Equal(wl.mintIntent, body) {
		t.Fatalf("intent must reach mint byte-for-byte:\nsent  %s\nminted %s", body, wl.mintIntent)
	}
	// The fetch leg (WORKLOAD-FETCH-CANVAS-ALIGN) rides the SAME ticket
	// and the SAME raw intent bytes, plus the delegation the mint just
	// returned — Gate re-validates all of it on the proxy call too.
	if wl.fetchCalls != 1 {
		t.Fatalf("expected exactly one canvas fetch, got %d", wl.fetchCalls)
	}
	if wl.fetchDelegation == nil || wl.fetchDelegation.JTI != "jti-1" || wl.fetchDelegation.AccessToken == "" || wl.fetchDelegation.GateRequestID == "" {
		t.Fatalf("fetch must receive the minted delegation (jti + access_token + gate_request_id), got %+v", wl.fetchDelegation)
	}
	if wl.fetchTicket != "opaque-ticket" {
		t.Fatalf("fetch must receive the relayed ticket, got %q", wl.fetchTicket)
	}
	if !bytes.Equal(wl.fetchIntent, body) {
		t.Fatalf("intent must reach the canvas fetch byte-for-byte:\nsent    %s\nfetched %s", body, wl.fetchIntent)
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

func TestPostSceneIntent_InlineValidatedCapsuleSkipsCanvasFetch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	_, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)
	wl := &fakeWorkload{}
	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      wl,
		Host:          bluehost.NewHost(),
	}
	body, err := json.Marshal(sceneIntentRequest{
		IntentID:          "intent-1",
		StreamID:          "stream-1",
		Target:            "preview",
		Action:            string(attestation.ActionPreparePreview),
		ResolvedSceneRef:  ref,
		BlueProgram:       base64.StdEncoding.EncodeToString(program),
		BlueProgramDigest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "opaque-ticket")

	rec := httptest.NewRecorder()
	postSceneIntent(deps)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if wl.fetchCalls != 0 {
		t.Fatalf("expected validated inline capsule to skip Canvas dereference, got %d fetches", wl.fetchCalls)
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
	if wl.mintCalls != 1 {
		t.Fatalf("expected 1 workload mint on first request, got %d", wl.mintCalls)
	}

	second := send()
	if second.Code != http.StatusOK {
		t.Fatalf("second request: expected 200, got %d: %s", second.Code, second.Body.String())
	}
	if wl.mintCalls != 1 {
		t.Fatalf("expected the replay to skip re-execution (still 1 workload mint), got %d", wl.mintCalls)
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
	if wl.mintCalls != 2 {
		t.Fatalf("expected every request without an idempotency_key to re-execute, got %d mints", wl.mintCalls)
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
	artifactSetDigest := "sha256:" + strings.Repeat("b", 64)
	host.SetArtifactSetDigest(bluehost.SlotPreview, artifactSetDigest)
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
	if resp.Preview.ArtifactSetDigest != artifactSetDigest {
		t.Fatalf("expected preview artifact_set_digest %q, got %+v", artifactSetDigest, resp.Preview)
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
