package api

// ORION-NOBLUE-AND-VERSION-ALIGN (#398) — two halves of one coherent
// change, proven together:
//
//  1. A ResolvedSceneRef whose SIGNED claims declare NO Blue program
//     (empty blue_program_digest) is accepted: the slot is occupied with
//     (scene_id, version) and the bundle is served, with NO runtime
//     instance ever Loaded/Started.
//  2. M6 — the version announced to Solar over the LSDP wire, the version
//     the Host entry stores, and the version the public resolver matches
//     are ONE value: claims.ArtifactSetDigest — the same value ZabCanvas
//     stamps as the bundle's own scene_version, which @lumencast/runtime
//     compares ?v= against client-side (bundle.ts:353).

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

// fixtureArtifactSetDigest is signedRef's fixed artifact_set_digest claim
// — the aligned serving version every assertion in this file pivots on.
const noblueTestVersionHex = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func noblueVersion() string { return "sha256:" + noblueTestVersionHex }

// alignedBundle builds LSML-bundle bytes that carry their OWN
// scene_version equal to claims.ArtifactSetDigest — exactly what the
// ZabCanvas twin emits (scene_digest = artifact_set_digest = the value
// stamped into the bundle). The client-side lumencast check compares this
// embedded field against the ?v= it fetched with; the three-way equality
// asserted below is what makes that check pass.
func alignedBundle(t *testing.T) []byte {
	t.Helper()
	bundle, err := json.Marshal(map[string]any{
		"scene_version": noblueVersion(),
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
// with the aligned version, serves its bundle through the public
// resolver — and holds NO runtime instance (every instance-consuming
// door answers "not loaded", and no program bytes ever existed to Load;
// had the handler attempted a Load, the empty program would have failed
// the intent with HOST_PREPARE_FAILED instead of this 200).
func TestPostSceneIntent_NoProgram_PrepareOccupiesAndServes_NoInstance(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	bundle := alignedBundle(t)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", "")
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

	// Slot occupied under the aligned (scene_id, version) identity.
	if !deps.Host.Serving(bluehost.SlotPreview, "scene-1", noblueVersion()) {
		t.Fatalf("expected preview slot serving (scene-1, %s)", noblueVersion())
	}
	// Bundle attached and served through the public resolver.
	if got := deps.Host.Bundle(bluehost.SlotPreview); string(got) != string(bundle) {
		t.Fatalf("bundle not attached: %q", got)
	}
	if fetched := fetchBundle(deps, "scene-1", noblueVersion()); fetched.Code != http.StatusOK || fetched.Body.String() != string(bundle) {
		t.Fatalf("public resolver must serve the bundle for the aligned v: %d %q", fetched.Code, fetched.Body.String())
	}
	// NO instance: every instance door answers not-loaded, never a panic
	// or a silent no-op.
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
	// fetch) — with the aligned version — but no bridge is stepping.
	if *gotVersion != noblueVersion() {
		t.Fatalf("MirrorFor announced %q, want %q", *gotVersion, noblueVersion())
	}
	if deps.Bridges.Running(bluehost.SlotPreview) {
		t.Fatal("no bridge may run for a program-less occupation")
	}
}

// TestPostSceneIntent_NoProgram_TakeSupersedesProgrammedInstance is
// proof (d)'s on-air half plus the supersede edge: a no-program take
// replaces a PROGRAMMED on-air occupation — the old bridge is stopped
// (nothing may keep stepping a released instance), the slot re-keys to
// the static identity, and the new bundle is served.
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
	refNo := signedRef(t, priv, "canvas-key-1", attestation.ActionTakeOnAir, now, "scene-2", "")
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
	if !deps.Host.Serving(bluehost.SlotOnAir, "scene-2", noblueVersion()) {
		t.Fatalf("expected on-air slot re-keyed to (scene-2, %s)", noblueVersion())
	}
	if deps.Bridges.Running(bluehost.SlotOnAir) {
		t.Fatal("superseded bridge must be STOPPED — no goroutine may keep stepping a released instance")
	}
	if *gotVersion != noblueVersion() {
		t.Fatalf("MirrorFor announced %q, want %q", *gotVersion, noblueVersion())
	}
	if fetched := fetchBundle(deps, "scene-2", noblueVersion()); fetched.Code != http.StatusOK {
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
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", "")
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

// TestPostSceneIntent_VersionAlignedEndToEnd_BothCases is proof (c): for
// a ref WITH a program and a ref WITHOUT one, the v announced on the
// wire == the scene_version the bundle itself carries == the ?v= the
// public resolver accepts (and serves as ETag) — ONE value end to end,
// claims.ArtifactSetDigest. The pre-M6 value (claims.SceneDigest) must
// no longer resolve: that is the exact mismatch M6 closes.
func TestPostSceneIntent_VersionAlignedEndToEnd_BothCases(t *testing.T) {
	sceneDigestClaim := "sha256:" + strings.Repeat("a", 64) // signedRef's fixed scene_digest — the pre-M6 announced value

	cases := []struct {
		name        string
		makeBody    func(t *testing.T) json.RawMessage
		programless bool
	}{
		{
			name: "with-program",
			makeBody: func(t *testing.T) json.RawMessage {
				program := minimalProgram(t)
				bundle := alignedBundle(t)
				digest := sha256Digest(program)
				body, _ := json.Marshal(map[string]any{
					"blue_program":        base64.StdEncoding.EncodeToString(program),
					"blue_program_digest": digest,
					"lsml_bundle":         base64.StdEncoding.EncodeToString(bundle),
					"lsml_bundle_digest":  sha256Digest(bundle),
				})
				return body
			},
		},
		{
			name: "no-program",
			makeBody: func(t *testing.T) json.RawMessage {
				return noProgramEnvelope(alignedBundle(t))
			},
			programless: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub, priv, _ := ed25519.GenerateKey(nil)
			now := time.Now()
			body := tc.makeBody(t)
			programDigest := ""
			if !tc.programless {
				var env resolvedSceneEnvelope
				if err := json.Unmarshal(body, &env); err != nil {
					t.Fatal(err)
				}
				programDigest = env.BlueProgramDigest
			}
			ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", programDigest)
			deps, gotVersion, mirrorCalls := noblueDeps(t, pub, body)
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
			if announced != noblueVersion() {
				t.Fatalf("wire announced %q, want claims.ArtifactSetDigest %q", announced, noblueVersion())
			}
			// (2) == the bundle's OWN scene_version (the field lumencast
			// compares ?v= against client-side).
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
			// The pre-M6 value must NOT resolve anymore.
			if miss := fetchBundle(deps, "scene-1", sceneDigestClaim); miss.Code != http.StatusNotFound {
				t.Fatalf("pre-M6 claims.SceneDigest %q must no longer resolve, got %d", sceneDigestClaim, miss.Code)
			}
		})
	}
}

// TestPostSceneIntent_NoProgram_PreviewOnAirSymmetry is proof (d): the
// same no-program ref occupies preview and on-air identically — same
// identity, same bundle, both resolvable through the public route,
// neither running a bridge.
func TestPostSceneIntent_NoProgram_PreviewOnAirSymmetry(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	bundle := alignedBundle(t)
	now := time.Now()
	deps, _, _ := noblueDeps(t, pub, noProgramEnvelope(bundle))

	refPrep := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", "")
	if rec := sendIntent(t, deps, refPrep, attestation.ActionPreparePreview, "preview", "i-prev"); rec.Code != http.StatusOK {
		t.Fatalf("prepare: %d %s", rec.Code, rec.Body.String())
	}
	refTake := signedRef(t, priv, "canvas-key-1", attestation.ActionTakeOnAir, now, "scene-1", "")
	if rec := sendIntent(t, deps, refTake, attestation.ActionTakeOnAir, "on-air", "i-take"); rec.Code != http.StatusOK {
		t.Fatalf("take: %d %s", rec.Code, rec.Body.String())
	}

	for _, slot := range []bluehost.Slot{bluehost.SlotPreview, bluehost.SlotOnAir} {
		if !deps.Host.Serving(slot, "scene-1", noblueVersion()) {
			t.Fatalf("%s: expected (scene-1, %s)", slot, noblueVersion())
		}
		if got := deps.Host.Bundle(slot); string(got) != string(bundle) {
			t.Fatalf("%s: bundle mismatch", slot)
		}
		if deps.Bridges.Running(slot) {
			t.Fatalf("%s: no bridge may run for a program-less occupation", slot)
		}
	}
	if fetched := fetchBundle(deps, "scene-1", noblueVersion()); fetched.Code != http.StatusOK {
		t.Fatalf("resolver: %d", fetched.Code)
	}
}
