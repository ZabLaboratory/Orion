package lsdp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	lproto "github.com/Lumencast/lumencast-go/protocol"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// sessionRoute mounts a test session's per-session preview LSDP wire on an
// httptest server, replicating internal/api/show.go's testSessionLSDP
// handler (ConnectWire + path rewrite onto the kit's /lsdp.v1). Returns
// the ws URL.
func sessionRoute(t *testing.T, mgr *runtime.TestSessionManager, sessionID string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, err := mgr.ConnectWire(sessionID)
		if err != nil {
			http.Error(w, "gone", http.StatusGone)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/lsdp.v1"
		handler.ServeHTTP(w, r2)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// expectNoFrame asserts the connection stays SILENT for the window. A
// frame arriving is the leak regression. NOTE: coder/websocket fails the
// connection when a Read context expires, so this MUST be the terminal
// operation on c — no further reads after it.
func expectNoFrame(t *testing.T, c *websocket.Conn, window time.Duration, who string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	_, raw, err := c.Read(ctx)
	if err == nil {
		t.Fatalf("%s emitted a frame %q after a cross-side change — isolation broken", who, raw)
	}
}

// TestLSDP_PreviewSessionDoesNotLeakToAntenne is the verrou proof: a live
// show is active on scene "scene-1" with an LSDP subscriber attached
// (= the antenne), and a test session is opened on the SAME scene id with
// its own preview LSDP subscriber. A change pushed into the test session
// MUST surface on the per-session route and MUST NOT emit any frame on the
// global /show/stream.lsdp wire (no scene_changed, no delta) — the
// preview/antenne split. The shared scene id proves the per-SESSION mirror
// keying (not per-sceneID) avoids the kit-scene collision.
func TestLSDP_PreviewSessionDoesNotLeakToAntenne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- Antenne: global show + LSDP wire, active on scene-1. ---
	wire, globalScene, globalWsURL := dualShow(t)

	// --- Preview: a test session on the SAME scene id, own LSDP wire. ---
	logger := quietLogger(t)
	mgr := runtime.NewTestSessionManager(runtime.NewComputeRegistry(), logger, 5*time.Minute)
	mgr.SetSessionWires(NewSessionWireFactory(logger, nil))
	t.Cleanup(mgr.Close)

	sessionID, sessionScene := mgr.Open(
		ctx, "scene-1", passthroughGraph("scene-1", "sha256:test-1", "score.team_a"), &compiler.RenderBundle{SceneVersion: "sha256:test-1"},
	)
	if sessionID == "" {
		t.Fatal("Open returned empty session id")
	}
	sessionWsURL := sessionRoute(t, mgr, sessionID)

	// Antenne subscriber attaches and drains its keyframe (state at 0).
	antenne := dialLSDP(ctx, t, globalWsURL, "viewer", 0)
	defer antenne.Close(websocket.StatusNormalClosure, "")
	if snap, ok := readServerFrame(ctx, t, antenne).(*lproto.Snapshot); !ok {
		t.Fatalf("antenne first frame must be a snapshot")
	} else if string(snap.State["score.team_a"]) != "0" {
		t.Fatalf("antenne keyframe state = %v, want score.team_a=0", snap.State)
	}

	// Preview subscriber attaches and drains its keyframe.
	preview := dialLSDP(ctx, t, sessionWsURL, "operator", 0)
	defer preview.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, preview).(*lproto.Snapshot); !ok {
		t.Fatalf("preview first frame must be a snapshot")
	}

	// --- The change: push ONLY into the test session. ---
	if !sessionScene.Input(runtime.InputMsg{
		Path:  "score.team_a",
		Value: json.RawMessage(`14`),
	}) {
		t.Fatal("session scene inbox full")
	}

	// Preview route receives the change.
	delta, ok := readServerFrame(ctx, t, preview).(*lproto.Delta)
	if !ok {
		t.Fatalf("preview route must receive the session delta")
	}
	var got string
	for _, p := range delta.Patches {
		if p.Path == "score.team_a" {
			got = string(p.Value)
		}
	}
	if got != "14" {
		t.Fatalf("preview delta patch = %q, want 14", got)
	}

	// Reverse direction: a change on the global scene reaches the antenne.
	// If the session leaked onto the global wire, the antenne's FIRST queued
	// frame would be the preview's 14 (it precedes this 21) — so the value
	// assertion below also catches a leak.
	if !globalScene.Input(runtime.InputMsg{
		Path:  "score.team_a",
		Value: json.RawMessage(`21`),
	}) {
		t.Fatal("global scene inbox full")
	}
	if d, ok := readServerFrame(ctx, t, antenne).(*lproto.Delta); !ok {
		t.Fatalf("antenne must receive its own scene's delta")
	} else {
		var g string
		for _, p := range d.Patches {
			if p.Path == "score.team_a" {
				g = string(p.Value)
			}
		}
		if g != "21" {
			t.Fatalf("antenne delta patch = %q, want 21 (a 14 here means the preview leaked onto the antenne)", g)
		}
	}

	// The mirror tap into the global kit scene reflects only the antenne's
	// own value (21), never the preview's 14 — the kit stores never crossed.
	waitForState(t, wire, globalScene, "score.team_a", "21")

	// Terminal silence checks (each fails its own connection, so they run
	// last): no further queued frame on either side after its legit delta.
	expectNoFrame(t, antenne, 400*time.Millisecond, "antenne /show/stream.lsdp")
	expectNoFrame(t, preview, 400*time.Millisecond, "preview test.lsdp")
}
