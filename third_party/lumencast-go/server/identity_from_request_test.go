package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Lumencast/lumencast-go/protocol"
	"github.com/Lumencast/lumencast-go/server"
)

// startTestServerWithConfig launches a Server from a full Config (the
// caller fills Auth and/or IdentityFromRequest) and returns its base
// WebSocket URL. It mirrors startTestServer's run/teardown plumbing so
// the header-trust seam exercises the exact same lifecycle as the token
// path.
func startTestServerWithConfig(t *testing.T, cfg server.Config) (*server.Server, string) {
	t.Helper()
	cfg.ListenAddr = "127.0.0.1:0"
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 5 * time.Second
	}
	if cfg.SubscribeTimeout == 0 {
		cfg.SubscribeTimeout = 2 * time.Second
	}
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		_ = srv.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("server never reported Addr()")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return srv, "ws://" + srv.Addr() + "/lsdp.v1"
}

// dialWithHeader dials the LSDP endpoint with extra HTTP headers on the
// upgrade request — the channel a trusted front-proxy uses to convey an
// already-authenticated principal to the header-trust seam.
func dialWithHeader(t *testing.T, url string, header http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{protocol.SubProtocol},
		HTTPClient:   &http.Client{Timeout: 2 * time.Second},
		HTTPHeader:   header,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

// headerTrust reads the test header X-Test-User and maps it to an
// operator Identity. An empty header yields an anonymous Identity, which
// the server rejects — proving the seam still gates unauthenticated
// callers.
func headerTrust(r *http.Request) (server.Identity, error) {
	switch r.Header.Get("X-Test-User") {
	case "op":
		return server.Identity{Subject: "op", Role: protocol.RoleOperator}, nil
	case "viewer":
		return server.Identity{Subject: "v", Role: protocol.RoleViewer}, nil
	case "":
		return server.Anonymous(), errors.New("no X-Test-User header")
	default:
		return server.Anonymous(), errors.New("unknown X-Test-User")
	}
}

// --- New() validation -------------------------------------------------

func TestNew_AcceptsIdentityFromRequestAlone(t *testing.T) {
	srv, err := server.New(server.Config{
		ListenAddr:          "127.0.0.1:0",
		IdentityFromRequest: headerTrust,
	})
	if err != nil {
		t.Fatalf("IdentityFromRequest alone must be accepted: %v", err)
	}
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestNew_RejectsBothAuthSourcesNil(t *testing.T) {
	_, err := server.New(server.Config{ListenAddr: "127.0.0.1:0"})
	if err == nil {
		t.Fatal("Config with neither Auth nor IdentityFromRequest must error")
	}
}

func TestNew_AcceptsAuthAlone(t *testing.T) {
	// Backward-compat guard : the historical "Auth only" Config stays valid.
	_, err := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Auth:       opAuth(),
	})
	if err != nil {
		t.Fatalf("Auth alone must remain valid: %v", err)
	}
}

// --- Token path unchanged --------------------------------------------

// TestTokenPath_UnchangedWithoutSeam asserts the default (Auth-only)
// configuration produces the exact same subscribe → snapshot → input →
// delta sequence as before the seam was introduced. This is the
// non-breaking guarantee : with IdentityFromRequest nil, nothing about
// the token path moves.
func TestTokenPath_UnchangedWithoutSeam(t *testing.T) {
	srv, url := startTestServerWithConfig(t, server.Config{Auth: opAuth()})
	scene := srv.NewScene("main", server.WithDeclaredInputs([]string{"__inputs.title"}))
	_ = scene.Set(map[string]any{"__inputs.title": ""})

	c := dial(t, url)
	send(t, c, &protocol.Subscribe{Token: "op-tok"})

	snap, ok := recv(t, c).(*protocol.Snapshot)
	if !ok {
		t.Fatalf("first frame is not snapshot")
	}
	if snap.Seq != 1 || snap.SceneID != "main" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	send(t, c, &protocol.Input{
		Patches: []protocol.Patch{{Path: "__inputs.title", Value: json.RawMessage(`"hi"`)}},
	})
	d, ok := recv(t, c).(*protocol.Delta)
	if !ok {
		t.Fatalf("expected delta echo")
	}
	if d.Seq != 2 || len(d.Patches) != 1 || d.Patches[0].Path != "__inputs.title" {
		t.Fatalf("unexpected delta: %+v", d)
	}
}

// TestTokenPath_StillRejectsBadToken proves the Auth path still validates
// the token when the seam is absent.
func TestTokenPath_StillRejectsBadToken(t *testing.T) {
	srv, url := startTestServerWithConfig(t, server.Config{Auth: opAuth()})
	srv.NewScene("main")
	_ = srv.ActiveScene().Set(map[string]any{"x": 1})

	c := dial(t, url)
	send(t, c, &protocol.Subscribe{Token: "nope"})
	e, ok := recv(t, c).(*protocol.Error)
	if !ok {
		t.Fatalf("got %T, want *Error", e)
	}
	if e.Code != string(protocol.CodeAuthDenied) {
		t.Fatalf("got code %q", e.Code)
	}
}

// --- Header-trust path ------------------------------------------------

// TestHeaderTrust_SnapshotDeltaResume drives the full lifecycle through
// the seam : a valid header yields an operator Identity, the connection
// gets snapshot + delta, and a reconnect with since_sequence resumes
// from the replay buffer. The Subscribe carries a bogus token to prove
// it is ignored when IdentityFromRequest is set.
func TestHeaderTrust_SnapshotDeltaResume(t *testing.T) {
	srv, url := startTestServerWithConfig(t, server.Config{IdentityFromRequest: headerTrust})
	scene := srv.NewScene("main", server.WithDeclaredInputs([]string{"__inputs.title"}))
	_ = scene.Set(map[string]any{"__inputs.title": ""})

	h := http.Header{}
	h.Set("X-Test-User", "op")

	c := dialWithHeader(t, url, h)
	// Bogus token : there is no such token in any Authenticator (none is
	// even configured). If the server validated it, auth would fail.
	send(t, c, &protocol.Subscribe{Token: "this-token-is-never-validated"})

	snap, ok := recv(t, c).(*protocol.Snapshot)
	if !ok {
		t.Fatalf("first frame is not snapshot")
	}
	if snap.Seq != 1 {
		t.Fatalf("snapshot seq: got %d want 1", snap.Seq)
	}

	// Operator role from the header lets us write — proving the Identity
	// (not just authentication) flows through from the seam.
	send(t, c, &protocol.Input{
		Patches: []protocol.Patch{{Path: "__inputs.title", Value: json.RawMessage(`"hi"`)}},
	})
	d, ok := recv(t, c).(*protocol.Delta)
	if !ok || d.Seq != 2 {
		t.Fatalf("expected delta seq=2, got %+v", d)
	}
	_ = c.CloseNow()

	// Resume with since_sequence=1 → replay buffer ships delta(seq=2)
	// instead of a fresh snapshot, identical to the token path.
	c2 := dialWithHeader(t, url, h)
	send(t, c2, &protocol.Subscribe{Token: "still-ignored", SinceSequence: 1})
	d2, ok := recv(t, c2).(*protocol.Delta)
	if !ok {
		t.Fatalf("expected delta on resume, got non-delta")
	}
	if d2.Seq != 2 {
		t.Fatalf("resume delta seq: got %d want 2", d2.Seq)
	}
}

// TestHeaderTrust_IgnoresValidTokenIdentity is the sharpest proof that
// the token is not consulted : an Authenticator IS configured alongside
// the seam, and the Subscribe carries a real viewer token. If the token
// were honoured the connection would be a viewer (cannot write). Because
// the header maps to operator, the input is accepted.
func TestHeaderTrust_IgnoresValidTokenIdentity(t *testing.T) {
	srv, url := startTestServerWithConfig(t, server.Config{
		Auth:                opAuth(),
		IdentityFromRequest: headerTrust,
	})
	scene := srv.NewScene("main", server.WithDeclaredInputs([]string{"__inputs.title"}))
	_ = scene.Set(map[string]any{"__inputs.title": ""})

	h := http.Header{}
	h.Set("X-Test-User", "op")

	c := dialWithHeader(t, url, h)
	// viewer-tok is a real, valid token — but for a viewer, who cannot
	// write. The seam must override it with the operator from the header.
	send(t, c, &protocol.Subscribe{Token: "viewer-tok"})
	_ = recv(t, c) // snapshot

	send(t, c, &protocol.Input{
		Patches: []protocol.Patch{{Path: "__inputs.title", Value: json.RawMessage(`"hi"`)}},
	})
	frame := recv(t, c)
	if e, ok := frame.(*protocol.Error); ok {
		t.Fatalf("write rejected (%s) — token identity leaked through the seam", e.Code)
	}
	if _, ok := frame.(*protocol.Delta); !ok {
		t.Fatalf("got %T, want *Delta — operator from header should write", frame)
	}
}

// TestHeaderTrust_RejectsUnauthenticated confirms the seam still gates :
// a missing header yields an error from the func and the server closes
// with AUTH_DENIED, just like a bad token would.
func TestHeaderTrust_RejectsUnauthenticated(t *testing.T) {
	srv, url := startTestServerWithConfig(t, server.Config{IdentityFromRequest: headerTrust})
	srv.NewScene("main")
	_ = srv.ActiveScene().Set(map[string]any{"x": 1})

	c := dial(t, url) // no X-Test-User header
	send(t, c, &protocol.Subscribe{Token: "irrelevant"})
	e, ok := recv(t, c).(*protocol.Error)
	if !ok {
		t.Fatalf("got %T, want *Error", e)
	}
	if e.Code != string(protocol.CodeAuthDenied) {
		t.Fatalf("got code %q, want %s", e.Code, protocol.CodeAuthDenied)
	}
}
