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

// TestSceneIntent_PassesLSMLSourceDirectlyToMirrorFor proves source bytes
// reach the LSDP leaf gate and snapshot without compiling or storing a
// Solar RenderBundle during the scene switch.
func TestSceneIntent_PassesLSMLSourceDirectlyToMirrorFor(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	lsmlBundle := []byte(`{"kind":"text","bind":{"value":"board.display"}}`)
	envelope, digest := canvasEnvelopeWithBundle(program, lsmlBundle)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, now, "scene-1", digest)

	mirror := &recordingMirror{}
	var gotSceneID string
	var gotSceneVersion string
	var gotSlot bluehost.Slot
	var gotSource []byte
	var calls int
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
			t.Fatal("scene-intent must not compile source")
			return nil, nil, nil
		},
		MirrorFor: func(sceneID, sceneVersion string, slot bluehost.Slot, source []byte) runtime.SceneMirror {
			gotSceneID = sceneID
			gotSceneVersion = sceneVersion
			gotSlot = slot
			gotSource = append([]byte(nil), source...)
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
	if gotSlot != bluehost.SlotPreview {
		t.Fatalf("a prepare-preview must reach MirrorFor with slot=preview (#398) — got %q", gotSlot)
	}
	if gotSource == nil {
		t.Fatal("MirrorFor received no LSML source")
	}
	if string(gotSource) != string(lsmlBundle) {
		t.Fatalf("MirrorFor source mismatch: got %q, want %q", gotSource, lsmlBundle)
	}
	wantVersion := "sha256:" + strings.Repeat("b", 64)
	if gotSceneVersion != wantVersion || mirror.snapshotVersion != wantVersion {
		t.Fatalf("LSDP source/snapshot version = %q/%q, want artifact_set_digest %q", gotSceneVersion, mirror.snapshotVersion, wantVersion)
	}
	if got := deps.Host.Bundle(bluehost.SlotPreview); len(got) != 0 {
		t.Fatalf("LSML source must not be stored as a legacy RenderBundle: %q", got)
	}
	if compilerCalls != 0 {
		t.Fatalf("scene switch invoked static compiler %d times", compilerCalls)
	}
}

// TestStartBridge_ThreadsOnAirSlotIntoMirrorFor is
// TestStartBridge_ThreadsHostBundleIntoMirrorFor's symmetric case (#398
// resolution criterion): a take must reach MirrorFor with
// slot == bluehost.SlotOnAir. Together the two tests prove the flux
// (preview vs antenne) is threaded correctly in BOTH directions — the
// exact guarantee #398 requires and the reason a one-sided test is
// insufficient.
func TestStartBridge_ThreadsOnAirSlotIntoMirrorFor(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	envelope, digest := canvasEnvelope(program)
	now := time.Now()
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionTakeOnAir, now, "scene-1", digest)

	mirror := &recordingMirror{}
	var gotSlot bluehost.Slot
	var calls int

	deps := SceneIntentDeps{
		Trust:         attestation.TrustSet{"canvas-key-1": pub},
		LocatorPrefix: "scenes/",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		Workload:      &fakeWorkload{body: envelope},
		Host:          bluehost.NewHost(),
		MirrorFor: func(_, _ string, slot bluehost.Slot, _ []byte) runtime.SceneMirror {
			gotSlot = slot
			calls++
			return mirror
		},
		Bridges:            bluewire.NewRegistry(),
		ProjectionInterval: time.Hour,
	}

	body, _ := json.Marshal(sceneIntentRequest{
		IntentID: "intent-take-1", StreamID: "stream-1", Target: "on-air",
		Action: string(attestation.ActionTakeOnAir), ResolvedSceneRef: ref,
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
	if gotSlot != bluehost.SlotOnAir {
		t.Fatalf("a take must reach MirrorFor with slot=on-air (#398) — got %q", gotSlot)
	}
}
