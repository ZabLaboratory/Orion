package api

// ORION-NOBLUE-AND-VERSION-ALIGN (#398) — a scene without a Blue program
// is signed and airable (porteur's Decision A + Bastion's spec):
//
//  1. A ResolvedSceneRef whose SIGNED claims declare NO program (empty
//     blue_program_digest — the ONLY claims relaxation) is accepted: the
//     slot is occupied with (scene_id, scene_digest) and the bundle is
//     served, with NO runtime instance ever Loaded/Started/Stepped.
//  2. M6, no-program case only: ZabCanvas mints scene_digest ==
//     artifact_set_digest == the bundle hash and stamps that same value
//     as the bundle's own scene_version — so the version announced on
//     the LSDP wire (claims.SceneDigest, unchanged threading), the value
//     the public resolver matches, the ETag, and the scene_version
//     @lumencast/runtime compares ?v= against are ONE value by
//     construction. The with-program misalignment is intentionally NOT
//     touched (separate chantier); the with-program path is asserted
//     byte-identical elsewhere in this package.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// noblueDigest is the single content address of the no-program fixtures
// (Decision A): the claims' scene_digest AND artifact_set_digest AND the
// scene_version embedded in the bundle bytes. Orion never recomputes it
// over the bundle (documented gap — the signed claim is the authority);
// what matters here is the END-TO-END EQUALITY of the announced /
// matched / embedded values.
func noblueDigest() string { return "sha256:" + strings.Repeat("f", 64) }

// alignedBundle builds LSML-bundle bytes carrying their OWN
// scene_version equal to the claims' digest — exactly what the ZabCanvas
// twin emits for a no-program scene. The client-side lumencast check
// compares this embedded field against the ?v= it fetched with.
func alignedBundle(t *testing.T) []byte {
	t.Helper()
	bundle, err := json.Marshal(map[string]any{
		"scene_version": noblueDigest(),
		"nodes":         []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// noProgramEnvelope is the `zabcanvas.resolved-scene.v1` slice the twin
// serves for a no-program scene: lsml_bundle + lsml_bundle_digest, NO
// program fields at all.
func noProgramEnvelope(bundle []byte) json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"lsml_bundle":        base64.StdEncoding.EncodeToString(bundle),
		"lsml_bundle_digest": sha256Digest(bundle),
	})
	return body
}

// signedRefNoProgram mints the Decision-A claim shape: an EMPTY
// blue_program_digest and scene_digest == artifact_set_digest ==
// noblueDigest() (the bundle's content address), everything else as
// signedRef.
func signedRefNoProgram(t *testing.T, priv ed25519.PrivateKey, kid string, now time.Time, sceneID string) string {
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
		"scene_digest":             noblueDigest(),
		"artifact_set_digest":      noblueDigest(),
		"blue_program_digest":      "",
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

func noblueDeps(t *testing.T, pub ed25519.PublicKey, body json.RawMessage) (SceneIntentDeps, *string, *int) {
	t.Helper()
	var gotVersion string
	var mirrorCalls int
	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: body},
		Host:          bluehost.NewHost(),
		MirrorFor: func(_, sceneVersion string, _ bluehost.Slot, _ []byte) runtime.SceneMirror {
			gotVersion = sceneVersion
			mirrorCalls++
			return &recordingMirror{}
		},
		Bridges:            bluewire.NewRegistry(),
		ProjectionInterval: time.Hour,
	}
	return deps, &gotVersion, &mirrorCalls
}

func sendIntent(t *testing.T, deps SceneIntentDeps, ref string, action attestation.Action, target, intentID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(sceneIntentRequest{
		IntentID:         intentID,
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

// fetchBundle exercises the PUBLIC resolver (resolveHostBundle via
// getRenderBundle) with an explicit (scene_id, v) pair — the same door
// Solar comes through after learning (sceneID, version) off the wire.
func fetchBundle(deps SceneIntentDeps, sceneID, v string) *httptest.ResponseRecorder {
	pub := PublicDeps{SceneIntent: &deps}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scenes/"+sceneID+"/render-bundle?v="+v, nil)
	req.SetPathValue("id", sceneID)
	rec := httptest.NewRecorder()
	getRenderBundle(pub)(rec, req)
	return rec
}

// TestPostSceneIntent_NoProgram_PrepareOccupiesAndServes_NoInstance is
// proof (a): a no-program ref passes Verify, occupies the preview slot
// with (scene_id, scene_digest), serves its bundle through the public
// resolver — and holds NO runtime instance (every instance-consuming
// door answers "not loaded"; no program bytes ever existed to Load, and
// had the handler attempted one, the empty program would have failed the
// intent with HOST_PREPARE_FAILED instead of this 200 — Bastion req 3).
func TestPostSceneIntent_NoProgram_PrepareOccupiesAndServes_NoInstance(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	bundle := alignedBundle(t)
	now := time.Now()
	ref := signedRefNoProgram(t, priv, "canvas-key-1", now, "scene-1")
	deps, gotVersion, _ := noblueDeps(t, pub, noProgramEnvelope(bundle))

	rec := sendIntent(t, deps, ref, attestation.ActionPreparePreview, "preview", "intent-nb-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("no-program prepare-preview: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp sceneIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "prepared" || resp.SceneID != "scene-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// Slot occupied under (scene_id, scene_digest).
	if !deps.Host.Serving(bluehost.SlotPreview, "scene-1", noblueDigest()) {
		t.Fatalf("expected preview slot serving (scene-1, %s)", noblueDigest())
	}
	// Bundle attached and served through the public resolver.
	if got := deps.Host.Bundle(bluehost.SlotPreview); string(got) != string(bundle) {
		t.Fatalf("bundle not attached: %q", got)
	}
	if fetched := fetchBundle(deps, "scene-1", noblueDigest()); fetched.Code != http.StatusOK || fetched.Body.String() != string(bundle) {
		t.Fatalf("public resolver must serve the bundle for v=scene_digest: %d %q", fetched.Code, fetched.Body.String())
	}
	// NO instance: every instance door answers not-loaded, never a panic
	// or a silent no-op (Bastion req 3 — no Load/Start/Step/Stop).
	if _, err := deps.Host.Call(bluehost.SlotPreview, "any", nil); !errors.Is(err, bluehost.ErrNotLoaded) {
		t.Fatalf("expected ErrNotLoaded from Call on a static occupation, got %v", err)
	}
	if _, err := deps.Host.Step(bluehost.SlotPreview); !errors.Is(err, bluehost.ErrNotLoaded) {
		t.Fatalf("expected ErrNotLoaded from Step on a static occupation, got %v", err)
	}
	if names := deps.Host.PendingAwaitNames(bluehost.SlotPreview); names != nil {
		t.Fatalf("expected no armed awaits on a static occupation, got %v", names)
	}
	// The scene IS registered on the wire (Solar must learn what to
	// fetch) — but no bridge is stepping.
	if *gotVersion != noblueDigest() {
		t.Fatalf("MirrorFor announced %q, want %q", *gotVersion, noblueDigest())
	}
	if deps.Bridges.Running(bluehost.SlotPreview) {
		t.Fatal("no bridge may run for a program-less occupation")
	}
}

// TestPostSceneIntent_NoProgram_TakeSupersedesProgrammedInstance: a
// no-program take replaces a PROGRAMMED on-air occupation — the old
// bridge is stopped (nothing may keep stepping a released instance), the
// slot re-keys to the static identity, and the new bundle is served.
func TestPostSceneIntent_NoProgram_TakeSupersedesProgrammedInstance(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelopeWithBundle(program, []byte(`{"scene":"old"}`))
	now := time.Now()
	deps, gotVersion, _ := noblueDeps(t, pub, envelope)
	t.Cleanup(func() {
		deps.Bridges.StopAll()
		_ = deps.Host.Release(bluehost.SlotOnAir, "test-cleanup")
	})

	// Seed: a normal programmed take, bridge running.
	refWith := signedRef(t, priv, "canvas-key-1", attestation.ActionTakeOnAir, now, "scene-1", digest)
	if rec := sendIntent(t, deps, refWith, attestation.ActionTakeOnAir, "on-air", "intent-seed"); rec.Code != http.StatusOK {
		t.Fatalf("seed programmed take: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !deps.Bridges.Running(bluehost.SlotOnAir) {
		t.Fatal("seed take should run the on-air bridge")
	}

	// Supersede with a no-program take.
	bundle := alignedBundle(t)
	deps.Workload = &fakeWorkload{body: noProgramEnvelope(bundle)}
	refNo := signedRefNoProgram(t, priv, "canvas-key-1", now, "scene-2")
	rec := sendIntent(t, deps, refNo, attestation.ActionTakeOnAir, "on-air", "intent-nb-take")
	if rec.Code != http.StatusOK {
		t.Fatalf("no-program take-on-air: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp sceneIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "taken" {
		t.Fatalf("expected \"taken\", got %+v", resp)
	}
	if !deps.Host.Serving(bluehost.SlotOnAir, "scene-2", noblueDigest()) {
		t.Fatalf("expected on-air slot re-keyed to (scene-2, %s)", noblueDigest())
	}
	if deps.Bridges.Running(bluehost.SlotOnAir) {
		t.Fatal("superseded bridge must be STOPPED — no goroutine may keep stepping a released instance")
	}
	if *gotVersion != noblueDigest() {
		t.Fatalf("MirrorFor announced %q, want %q", *gotVersion, noblueDigest())
	}
	if fetched := fetchBundle(deps, "scene-2", noblueDigest()); fetched.Code != http.StatusOK {
		t.Fatalf("on-air static occupation must resolve through the public route, got %d", fetched.Code)
	}
}

// TestPostSceneIntent_NoProgram_RefusesEnvelopeCarryingProgram is proof
// (e)'s new face: claims that sign NO program + an envelope that carries
// one = unattested bytes → refused fail-closed, slot untouched.
func TestPostSceneIntent_NoProgram_RefusesEnvelopeCarryingProgram(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, _ := canvasEnvelope(program) // envelope WITH program fields
	now := time.Now()
	ref := signedRefNoProgram(t, priv, "canvas-key-1", now, "scene-1")
	deps, _, _ := noblueDeps(t, pub, envelope)

	rec := sendIntent(t, deps, ref, attestation.ActionPreparePreview, "preview", "intent-nb-evil")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for unattested program bytes, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp sceneIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Reason != "CANVAS_ARTIFACT_DIGEST_MISMATCH" {
		t.Fatalf("expected CANVAS_ARTIFACT_DIGEST_MISMATCH, got %+v", resp)
	}
	if deps.Host.Digest(bluehost.SlotPreview) != "" {
		t.Fatal("refused intent must not occupy the slot")
	}
}

// TestPostSceneIntent_NoProgram_VersionAlignedEndToEnd is proof (c) for
// the no-program case (Decision A): the v announced on the wire == the
// scene_version the bundle itself carries == the ?v= the public resolver
// accepts (and serves as ETag) — one value end to end, with NO re-keying
// in Orion (the announced value is claims.SceneDigest, same threading as
// a programmed ref; the equality holds because ZabCanvas mints
// scene_digest as the bundle's content address). A non-matching v must
// still miss.
func TestPostSceneIntent_NoProgram_VersionAlignedEndToEnd(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	bundle := alignedBundle(t)
	ref := signedRefNoProgram(t, priv, "canvas-key-1", now, "scene-1")
	deps, gotVersion, mirrorCalls := noblueDeps(t, pub, noProgramEnvelope(bundle))
	t.Cleanup(func() {
		deps.Bridges.StopAll()
		_ = deps.Host.Release(bluehost.SlotPreview, "test-cleanup")
	})

	if rec := sendIntent(t, deps, ref, attestation.ActionPreparePreview, "preview", "intent-align"); rec.Code != http.StatusOK {
		t.Fatalf("prepare-preview: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if *mirrorCalls != 1 {
		t.Fatalf("expected exactly one wire registration, got %d", *mirrorCalls)
	}

	// (1) announced on the wire.
	announced := *gotVersion
	if announced != noblueDigest() {
		t.Fatalf("wire announced %q, want the scene_digest/bundle hash %q", announced, noblueDigest())
	}
	// (2) == the bundle's OWN scene_version (the field lumencast compares
	// ?v= against client-side).
	var served struct {
		SceneVersion string `json:"scene_version"`
	}
	fetched := fetchBundle(deps, "scene-1", announced)
	if fetched.Code != http.StatusOK {
		t.Fatalf("resolver must accept the announced v %q, got %d", announced, fetched.Code)
	}
	if err := json.Unmarshal(fetched.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	if served.SceneVersion != announced {
		t.Fatalf("bundle scene_version %q != announced/fetched v %q — the client-side lumencast check would refuse this bundle", served.SceneVersion, announced)
	}
	// (3) ETag is the same value (immutable-cache identity).
	if etag := fetched.Header().Get("ETag"); etag != `"`+announced+`"` {
		t.Fatalf("ETag %q != announced v %q", etag, announced)
	}
	// A non-matching v still misses — the resolver guard is intact.
	if miss := fetchBundle(deps, "scene-1", "sha256:"+strings.Repeat("a", 64)); miss.Code != http.StatusNotFound {
		t.Fatalf("a non-matching v must 404, got %d", miss.Code)
	}
}

// TestPostSceneIntent_NoProgram_PreviewOnAirSymmetry: the same
// no-program ref occupies preview and on-air identically — same
// identity, same bundle, both resolvable through the public route,
// neither running a bridge.
func TestPostSceneIntent_NoProgram_PreviewOnAirSymmetry(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	bundle := alignedBundle(t)
	now := time.Now()
	deps, _, _ := noblueDeps(t, pub, noProgramEnvelope(bundle))

	refPrep := signedRefNoProgram(t, priv, "canvas-key-1", now, "scene-1")
	if rec := sendIntent(t, deps, refPrep, attestation.ActionPreparePreview, "preview", "i-prev"); rec.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", rec.Code, rec.Body.String())
	}
	refTake := signedRefNoProgram(t, priv, "canvas-key-1", now, "scene-1")
	if rec := sendIntent(t, deps, refTake, attestation.ActionTakeOnAir, "on-air", "i-take"); rec.Code != http.StatusOK {
		t.Fatalf("take: %d %s", rec.Code, rec.Body.String())
	}

	for _, slot := range []bluehost.Slot{bluehost.SlotPreview, bluehost.SlotOnAir} {
		if !deps.Host.Serving(slot, "scene-1", noblueDigest()) {
			t.Fatalf("%s: expected (scene-1, %s)", slot, noblueDigest())
		}
		if got := deps.Host.Bundle(slot); string(got) != string(bundle) {
			t.Fatalf("%s: bundle mismatch", slot)
		}
		if deps.Bridges.Running(slot) {
			t.Fatalf("%s: no bridge may run for a program-less occupation", slot)
		}
	}
	if fetched := fetchBundle(deps, "scene-1", noblueDigest()); fetched.Code != http.StatusOK {
		t.Fatalf("resolver: %d", fetched.Code)
	}
}
