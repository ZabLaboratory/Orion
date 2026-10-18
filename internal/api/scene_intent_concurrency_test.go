package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type intentTestKey struct{}

func TestSceneIntentLaneCommitSerializesReplayButNotOtherLane(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	var envelope resolvedSceneEnvelope
	raw, digest := canvasEnvelope(program)
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	firstFlush := make(chan struct{})
	release := make(chan struct{})
	var flushes atomic.Int32
	deps := SceneIntentDeps{Trust: attestation.TrustSet{"canvas-key-1": pub}, LocatorPrefix: "scenes/", OwnerID: "owner-1", TenantID: "tenant-1", EmbeddedLocal: true, Host: bluehost.NewHost(), Idempotency: NewIdempotencyCache(),
		WireFlush: func(context.Context) error {
			if flushes.Add(1) == 1 {
				close(firstFlush)
				<-release
			}
			return nil
		},
	}
	handler := postSceneIntent(deps)
	replayHandler := postSceneIntent(deps) // wrapper-created handler, same Host.
	request := func(action attestation.Action, ctx context.Context) *httptest.ResponseRecorder {
		body, _ := json.Marshal(sceneIntentRequest{IntentID: "same-intent", StreamID: "stream-1", Action: string(action), ResolvedSceneRef: signedRef(t, priv, "canvas-key-1", action, time.Now(), "scene-1", digest), IdempotencyKey: "same-request", BlueProgram: envelope.BlueProgram, BlueProgramDigest: envelope.BlueProgramDigest})
		r := httptest.NewRequest("POST", "/api/v1/host/scene-intent", bytes.NewReader(body)).WithContext(ctx)
		r.Header.Set("X-Authenticated-User", "operator-1")
		r.Header.Set("X-Authenticated-Role", "operator")
		w := httptest.NewRecorder()
		if ctx.Value(intentTestKey{}) == true {
			replayHandler(w, r)
		} else {
			handler(w, r)
		}
		return w
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- request(attestation.ActionTakeOnAir, context.Background()) }()
	<-firstFlush
	// Preview commits while Program waits on the native receipt.
	preview := request(attestation.ActionPreparePreview, context.Background())
	if preview.Code != http.StatusOK {
		t.Fatal(preview.Code, preview.Body.String())
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := request(attestation.ActionTakeOnAir, cancelled); got.Code != http.StatusRequestTimeout {
		t.Fatal("cancelled lane wait", got.Code, got.Body.String())
	}
	replay := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		replay <- request(attestation.ActionTakeOnAir, context.WithValue(context.Background(), intentTestKey{}, true))
	}()
	select {
	case <-replay:
		t.Fatal("concurrent replay bypassed commit barrier")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if got := <-first; got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body.String())
	}
	if got := <-replay; got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body.String())
	}
	if flushes.Load() != 2 {
		t.Fatalf("replay restarted the Blue/bridge commit: %d", flushes.Load())
	}
}
