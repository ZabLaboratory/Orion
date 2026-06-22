package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// newRigWithAuthSource is newLiveRig but lets the test pin the WS
// Server's AuthSource — the same seam the HTTP gates use (ADR 016
// §3.2-2). It proves /show/stream derives identity through the
// configured source, not the static auth.FromHeaders.
func newRigWithAuthSource(t *testing.T, src auth.AuthSource) *liveTestRig {
	t.Helper()
	rig := newLiveRig(t)
	// Re-wire the handler around a Server carrying the chosen source.
	prevCleanup := rig.cleanup
	rig.srv.Close()
	srv := &Server{
		Show:       rig.show,
		Inbox:      rig.inbox,
		Test:       runtime.NewTestSessionManager(runtime.NewComputeRegistry(), quietLogger(), time.Minute),
		Logger:     quietLogger(),
		Metrics:    obs.NewMetrics(),
		AuthSource: src,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/show/stream", srv.ServeShowStream)
	httpSrv := httptest.NewServer(mux)
	rig.srv = httpSrv
	rig.wsURL = "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/show/stream"
	rig.cleanup = func() {
		httpSrv.Close()
		prevCleanup()
	}
	return rig
}

// readSnapshot subscribes and reads the initial snapshot frame,
// asserting the connection was accepted and identity-gated through.
func readSnapshot(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribe","v":1,"since_sequence":null}`)); err != nil {
		t.Fatal(err)
	}
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap protocol.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Type != protocol.TypeSnapshot {
		t.Fatalf("expected snapshot, got %s", raw)
	}
}

// TestWS_EmbeddedLocal_HandshakeAccepted proves the embedded-local
// AuthSource (localOperatorAuth) is honoured on /show/stream: a loopback
// caller presenting the X-Orion-Local-Auth handshake secret is granted
// the operator role and the subscription is accepted. Before the fix the
// WS read the static auth.FromHeaders, which sees no X-Authenticated-*
// header on the local path and closed the connection 401.
func TestWS_EmbeddedLocal_HandshakeAccepted(t *testing.T) {
	const secret = "prism-handshake-secret-xyz"
	src, err := auth.NewLocalOperatorAuth(secret, "local-operator")
	if err != nil {
		t.Fatal(err)
	}
	rig := newRigWithAuthSource(t, src)
	defer rig.cleanup()

	headers := http.Header{auth.HandshakeHeader: []string{secret}}
	c := dialWith(t, rig.wsURL, headers)
	defer c.Close(websocket.StatusNormalClosure, "")
	readSnapshot(t, c)
}

// TestWS_EmbeddedLocal_WrongSecretRejected proves fail-closed: a loopback
// caller without (or with a wrong) handshake secret falls back to the
// anonymous identity and is rejected with 401 — the local port is not an
// open operator door.
func TestWS_EmbeddedLocal_WrongSecretRejected(t *testing.T) {
	src, err := auth.NewLocalOperatorAuth("the-real-secret", "local-operator")
	if err != nil {
		t.Fatal(err)
	}
	rig := newRigWithAuthSource(t, src)
	defer rig.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	headers := http.Header{auth.HandshakeHeader: []string{"wrong-secret"}}
	_, resp, err := websocket.Dial(ctx, rig.wsURL, &websocket.DialOptions{HTTPHeader: headers})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected dial to fail (handshake rejected), got accepted connection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got resp=%v", resp)
	}
}

// TestWS_AntenneHeaderTrust_PreservedWithLocalSource proves prod parity is
// untouched and explicit: with the default HeaderAuthSource wired (the
// antenne profile), an operator identified by the ZabGate-injected
// X-Authenticated-* headers is still accepted exactly as before — the
// header-trust path is the one the WS uses on the antenne.
func TestWS_AntenneHeaderTrust_PreservedWithLocalSource(t *testing.T) {
	rig := newRigWithAuthSource(t, auth.HeaderAuthSource{})
	defer rig.cleanup()

	headers := http.Header{
		"X-Authenticated-User": []string{"user-1"},
		"X-Authenticated-Role": []string{"operator"},
	}
	c := dialWith(t, rig.wsURL, headers)
	defer c.Close(websocket.StatusNormalClosure, "")
	readSnapshot(t, c)
}

// TestWS_AntenneHeaderTrust_NoHeaderRejected proves the default path stays
// fail-closed: no trust header (and no handshake) ⇒ anonymous ⇒ 401.
func TestWS_AntenneHeaderTrust_NoHeaderRejected(t *testing.T) {
	rig := newRigWithAuthSource(t, auth.HeaderAuthSource{})
	defer rig.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, rig.wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected dial to fail (unauthenticated), got accepted connection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got resp=%v", resp)
	}
}
