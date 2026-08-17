package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// recordingBundleMirror is a bare runtime.SceneMirror stand-in — the
// bytes/sceneID/call-count assertions this test cares about are captured
// directly by the MirrorFor closure below, not by the mirror itself.
type recordingBundleMirror struct{}

func (m *recordingBundleMirror) Forward(any) {}

// TestStartBridge_ThreadsHostBundleIntoMirrorFor is the plumbing proof for
// #396: an envelope carrying an lsml_bundle is verified and attached to
// the slot via Host.SetBundle (scenes_intent.go's existing behaviour,
// unchanged), and startBridge must now pass exactly those bytes to
// deps.MirrorFor — the wiring that was missing before this fix, where
// cmd/orion/main.go's closure ignored whatever the slot held and always
// forwarded nil to lsdp.Wire.MirrorFor's bundle argument.
//
// Mutation proof: reverting scene_intent.go's `deps.MirrorFor(claims.SceneID,
// deps.Host.Bundle(slot))` back to the pre-fix `deps.MirrorFor(claims.SceneID)`
// (1-arg call site) does not even compile against the widened MirrorFor
// field — and reverting the field type too (to make it compile again)
// makes gotBundle below observe nil, failing this test's non-nil/equality
// assertions.
func TestStartBridge_ThreadsHostBundleIntoMirrorFor(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	lsmlBundle := []byte(`{"kind":"text","bind":{"value":"board.display"}}`)
	envelope, digest := canvasEnvelopeWithBundle(program, lsmlBundle)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

	mirror := &recordingBundleMirror{}
	var gotSceneID string
	var gotBundle []byte
	var calls int

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
		MirrorFor: func(sceneID string, bundle []byte) runtime.SceneMirror {
			gotSceneID = sceneID
			gotBundle = bundle
			calls++
			return mirror
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
		t.Errorf("expected sceneID %q, got %q", "scene-1", gotSceneID)
	}
	if gotBundle == nil {
		t.Fatalf("MirrorFor received a nil bundle — the #396 defect: SetBundle stored real LSML bytes but startBridge never forwarded them")
	}
	if string(gotBundle) != string(lsmlBundle) {
		t.Fatalf("MirrorFor bundle mismatch: got %q, want %q", gotBundle, lsmlBundle)
	}

	// Cross-check against the slot directly: what startBridge forwarded
	// must be exactly what SetBundle attached (deps.Host.Bundle(slot)),
	// never a copy or a different artefact.
	if got := deps.Host.Bundle(bluehost.SlotPreview); string(got) != string(lsmlBundle) {
		t.Fatalf("Host.Bundle(preview) = %q, want %q", got, lsmlBundle)
	}
}
