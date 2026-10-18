package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
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

// The admission callbacks key mirror generations/rosters by artifact_set_digest;
// Blue execution retains scene_digest. Native source-version preservation is
// covered by lsdpreception's source document tests and the cold-start CEF proof.
func TestPostSceneIntent_UsesArtifactSetForGenerationAndSceneDigestForBlue(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	source := []byte(`{"lsml":"1.1","layout":{"kind":"text"}}`)
	envelope, digest := canvasEnvelopeWithBundle(program, source)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

	var gotSceneID, gotSceneVersion string
	var gotRosterVersion, gotActiveVersion string
	var calls int
	mirror := &recordingMirror{}
	compilerCalls := 0
	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
		StaticBundleCompiler: func([]byte, string, string) ([]byte, map[string]json.RawMessage, error) {
			compilerCalls++
			t.Fatal("scene-intent must not compile LSML on a switch")
			return nil, nil, nil
		},
		MirrorFor: func(sceneID, sceneVersion string, _ bluehost.Slot, gotSource []byte) runtime.SceneMirror {
			gotSceneID = sceneID
			gotSceneVersion = sceneVersion
			calls++
			if !bytes.Equal(gotSource, source) {
				t.Fatalf("MirrorFor source = %s, want source bytes %s", gotSource, source)
			}
			return mirror
		},
		EmitRoster: func(_ bluehost.Slot, entries []runtime.RosterEntry) {
			if len(entries) == 1 {
				gotRosterVersion = entries[0].SceneVersion
			}
		},
		Activate:           func(_ string, version string, _ bluehost.Slot) { gotActiveVersion = version },
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
		t.Fatal("MirrorFor received an empty sceneVersion â€” the #398-class defect: a client can never learn a ?v= that resolveHostBundle would accept")
	}
	wantSceneVersion := "sha256:" + strings.Repeat("b", 64)
	if gotSceneVersion != wantSceneVersion || gotRosterVersion != wantSceneVersion || gotActiveVersion != wantSceneVersion || mirror.snapshotVersion != wantSceneVersion {
		t.Fatalf("Admitted generation identity mismatch: mirror=%q roster=%q snapshot=%q active=%q want artifact_set_digest=%q", gotSceneVersion, gotRosterVersion, mirror.snapshotVersion, gotActiveVersion, wantSceneVersion)
	}
	wantBlueDigest := "sha256:" + strings.Repeat("a", 64)
	if got := deps.Host.Digest(bluehost.SlotPreview); got != wantBlueDigest {
		t.Fatalf("Blue host digest = %q, want scene_digest %q", got, wantBlueDigest)
	}
	if compilerCalls != 0 {
		t.Fatalf("scene switch invoked static compiler %d times", compilerCalls)
	}
}
