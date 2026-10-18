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
	"testing"
	"time"
)

type releaseBlockedPortal struct {
	fakeWorkload
	entered, finish chan struct{}
}

func (p *releaseBlockedPortal) AdmitInline(ctx context.Context, _ string, _ json.RawMessage) error {
	close(p.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.finish:
		return nil
	}
}

func TestProgramAdmissionPreparedBeforeReleaseCannotPublishAfterStop(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := minimalProgram(t)
	raw, digest := canvasEnvelope(program)
	var envelope resolvedSceneEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	portal := &releaseBlockedPortal{entered: make(chan struct{}), finish: make(chan struct{})}
	host := bluehost.NewHost()
	deps := SceneIntentDeps{Trust: attestation.TrustSet{"canvas-key-1": pub}, LocatorPrefix: "scenes/", OwnerID: "owner-1", TenantID: "tenant-1", Workload: portal, Host: host}
	body, _ := json.Marshal(sceneIntentRequest{IntentID: "delayed-admission", StreamID: "stream-1", Action: "take-on-air", ResolvedSceneRef: signedRef(t, priv, "canvas-key-1", attestation.ActionTakeOnAir, time.Now(), "scene-1", digest), BlueProgram: envelope.BlueProgram, BlueProgramDigest: envelope.BlueProgramDigest})
	req := httptest.NewRequest("POST", "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "local-test-ticket")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { rec := httptest.NewRecorder(); postSceneIntent(deps)(rec, req); done <- rec }()
	select {
	case <-portal.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("admission did not reach external gate")
	}
	host.SuspendProgramAdmissions()
	close(portal.finish)
	got := <-done
	if got.Code != http.StatusConflict || !bytes.Contains(got.Body.Bytes(), []byte("PROGRAM_ADMISSION_SUPERSEDED")) || host.Digest(bluehost.SlotOnAir) != "" {
		t.Fatal(got.Code, got.Body.String())
	}
	// A new HTTP take cannot bypass the stopped lifecycle either.
	req = httptest.NewRequest("POST", "/api/v1/host/scene-intent", bytes.NewReader(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	req.Header.Set(authContextHeader, "local-test-ticket")
	got = httptest.NewRecorder()
	postSceneIntent(deps)(got, req)
	if got.Code != http.StatusConflict || !bytes.Contains(got.Body.Bytes(), []byte("PROGRAM_RELEASED")) {
		t.Fatal(got.Code, got.Body.String())
	}
}
