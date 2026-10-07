package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type rejectedPresentation struct {
	phase   string
	aborted bool
}

func (p *rejectedPresentation) Commit(context.Context) error {
	if p.phase == "commit" {
		return errors.New("no renderer ACK")
	}
	return nil
}
func (p *rejectedPresentation) Finalize(context.Context) error {
	if p.phase == "finalize" {
		return errors.New("finalization failed")
	}
	return nil
}
func (p *rejectedPresentation) Abort(ctx context.Context) error { p.aborted = true; return ctx.Err() }

func TestSceneIntentCompensatesEachPreFinalizationFailure(t *testing.T) {
	for _, phase := range []string{"prepare", "commit", "wire", "finalize"} {
		t.Run(phase, func(t *testing.T) {
			pub, priv, _ := ed25519.GenerateKey(nil)
			program := minimalProgram(t)
			envelope, digest := canvasEnvelopeWithBundle(program, []byte(`{"lsml":"1.2","scene_id":"scene-1","layout":{"type":"frame"},"defaults":{}}`))
			host := bluehost.NewHost()
			if err := host.PreparePreview("old", "old-scene", "old-digest", program, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			oldMirror := &recordingMirror{}
			bridges := bluewire.NewRegistry()
			oldBridge := bluewire.NewBridge(host, bluehost.SlotPreview, oldMirror, "old-scene", "old-digest", "old", "preview", "rev", "intent")
			bridges.Start(bluehost.SlotPreview, oldBridge, time.Hour, nil)
			defer bridges.StopAll()
			defer func() {
				if err := host.Release(bluehost.SlotPreview, "test"); err != nil {
					t.Error(err)
				}
			}()
			presentation := &rejectedPresentation{phase: phase}
			laneRestored := false
			wireCalls := 0
			deps := SceneIntentDeps{Trust: attestation.TrustSet{"canvas-key-1": pub}, LocatorPrefix: "scenes/", OwnerID: "owner-1", TenantID: "tenant-1", Host: host, Workload: &fakeWorkload{body: envelope}, Bridges: bridges,
				MirrorFor:           func(string, string, bluehost.Slot, []byte) runtime.SceneMirror { return &recordingMirror{} },
				BeginLaneTransition: func(bluehost.Slot) func(bool) { return func(restore bool) { laneRestored = restore } },
				PresentScene: func(context.Context, bluehost.Slot, string, string, string, []byte) (ScenePresentation, error) {
					if phase == "prepare" {
						return presentation, errors.New("invalid source")
					}
					return presentation, nil
				},
				WireFlush: func(context.Context) error {
					wireCalls++
					if phase == "wire" && wireCalls == 1 {
						return errors.New("native timeout")
					}
					return nil
				}}
			raw, _ := json.Marshal(sceneIntentRequest{IntentID: "new", StreamID: "stream-1", Action: "prepare-preview", ResolvedSceneRef: signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, time.Now(), "scene-1", digest)})
			request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
			request.Header.Set("X-Authenticated-User", "operator-1")
			request.Header.Set("X-Authenticated-Role", "operator")
			request.Header.Set(authContextHeader, "ticket")
			response := httptest.NewRecorder()
			postSceneIntent(deps)(response, request)
			if response.Code == http.StatusOK {
				t.Fatal("failed presentation reported success")
			}
			if !host.Serving(bluehost.SlotPreview, "old-scene", "old-digest") || bridges.Current(bluehost.SlotPreview) != oldBridge || !laneRestored || !presentation.aborted {
				t.Fatalf("compensation incomplete: %s", response.Body.String())
			}
		})
	}
}
