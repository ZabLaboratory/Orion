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

// dualShow builds a runtime.Show with the LSDP wire installed (dual
// mode), loads one passthrough scene, makes it active, and mounts the
// kit handler on an httptest server. Returns the show, the scene, and
// the ws URL of the LSDP route.
func dualShow(t *testing.T) (*Wire, *runtime.Scene, string) {
	t.Helper()
	logger := quietLogger(t)

	wire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}

	show := runtime.NewShow(runtime.NewComputeRegistry(), logger)
	show.SetMirrors(wire)
	t.Cleanup(show.Stop)

	graph := &compiler.Graph{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		Nodes: []compiler.GraphNode{
			{ID: "in.score", Kind: "input"},
			{ID: "out.score", Kind: "output", Path: "score.team_a", Compute: "core.passthrough", Upstream: []string{"in.score"}},
		},
		Defaults: map[string]json.RawMessage{
			"score.team_a": json.RawMessage(`0`),
		},
	}
	show.Load("scene-1", graph, &compiler.RenderBundle{SceneVersion: "sha256:test-1"})
	if err := show.SetActive("scene-1", nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	scene, err := show.Get("scene-1")
	if err != nil {
		t.Fatalf("Get scene: %v", err)
	}

	// Mount the kit handler; rewrite the path to /lsdp.v1 like the
	// public router does (api.lsdpRoute).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/lsdp.v1"
		wire.Handler().ServeHTTP(w, r2)
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return wire, scene, wsURL
}

// dialLSDP opens an LSDP/1.1 connection carrying ZabGate header-trust
// headers (operator). The Subscribe token is empty on purpose — the
// header-trust seam must ignore it.
func dialLSDP(ctx context.Context, t *testing.T, wsURL, role string, since uint64) *websocket.Conn {
	t.Helper()
	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{lproto.SubProtocolV1_1},
		HTTPHeader: http.Header{
			"X-Authenticated-User": {"user-abc"},
			"X-Authenticated-Role": {role},
		},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sub, _ := lproto.Encode(&lproto.Subscribe{SinceSequence: since}) // empty Token
	if err := c.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}
	return c
}

func readServerFrame(ctx context.Context, t *testing.T, c *websocket.Conn) any {
	t.Helper()
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	msg, err := lproto.DecodeServer(raw)
	if err != nil {
		t.Fatalf("decode server frame %q: %v", raw, err)
	}
	return msg
}

// TestLSDP_DualSnapshotDeltaResume is acceptance (4) of issue #25 in
// dual mode: an LSDP/1.1 client connects header-trust, gets a snapshot,
// a delta on operator input, and a since_sequence resume.
func TestLSDP_DualSnapshotDeltaResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, scene, wsURL := dualShow(t)

	c := dialLSDP(ctx, t, wsURL, "operator", 0)
	defer c.Close(websocket.StatusNormalClosure, "")

	// 1. Snapshot carries the seeded default + the scene version.
	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatalf("first frame: want snapshot")
	}
	if snap.SceneVersion != "sha256:test-1" {
		t.Fatalf("snapshot version = %q", snap.SceneVersion)
	}
	if string(snap.State["score.team_a"]) != "0" {
		t.Fatalf("snapshot state = %v", snap.State)
	}

	// 2. Operator input on the reactive scene → delta on the LSDP wire.
	if !scene.Input(runtime.InputMsg{
		Path:  "score.team_a",
		Value: json.RawMessage(`14`),
	}) {
		t.Fatal("scene inbox full")
	}
	delta, ok := readServerFrame(ctx, t, c).(*lproto.Delta)
	if !ok {
		t.Fatalf("second frame: want delta")
	}
	var got string
	for _, p := range delta.Patches {
		if p.Path == "score.team_a" {
			got = string(p.Value)
		}
	}
	if got != "14" {
		t.Fatalf("delta patch = %q (delta seq %d)", got, delta.Seq)
	}
	resumeSeq := delta.Seq
	_ = c.Close(websocket.StatusNormalClosure, "")

	// 3. since_sequence resume: reconnect asking for everything after
	// the last seen seq. A fresh input bumps the scene; the resume path
	// must replay the delta (not a snapshot) when the buffer covers it.
	if !scene.Input(runtime.InputMsg{
		Path:  "score.team_a",
		Value: json.RawMessage(`21`),
	}) {
		t.Fatal("scene inbox full")
	}
	// Give the mirror a beat to apply the emit before resuming.
	waitForState(t, wire, scene, "score.team_a", "21")

	c2 := dialLSDP(ctx, t, wsURL, "viewer", resumeSeq)
	defer c2.Close(websocket.StatusNormalClosure, "")
	resume := readServerFrame(ctx, t, c2)
	switch m := resume.(type) {
	case *lproto.Delta:
		// Replay path: deltas from resumeSeq+1 forward.
		if m.Seq <= resumeSeq {
			t.Fatalf("resume delta seq %d not after %d", m.Seq, resumeSeq)
		}
	case *lproto.Snapshot:
		// Acceptable fallback when the buffer doesn't cover the gap:
		// the snapshot must reflect the latest state.
		if string(m.State["score.team_a"]) != "21" {
			t.Fatalf("resume snapshot state = %v", m.State)
		}
	default:
		t.Fatalf("resume frame type %T", resume)
	}
}

// TestLSDP_TokenIgnored proves the header-trust seam wins: a Subscribe
// with an empty token but operator headers is accepted (no token path
// is consulted). A connection with NO auth headers is rejected.
func TestLSDP_TokenIgnored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, wsURL := dualShow(t)

	// Operator headers, empty token → accepted (snapshot).
	c := dialLSDP(ctx, t, wsURL, "operator", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("operator header-trust subscribe should yield a snapshot")
	}

	// No auth headers → Anonymous identity → AUTH_DENIED.
	cNo, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{lproto.SubProtocolV1_1},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cNo.Close(websocket.StatusNormalClosure, "")
	sub, _ := lproto.Encode(&lproto.Subscribe{})
	if err := cNo.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatalf("write: %v", err)
	}
	errFrame, ok := readServerFrame(ctx, t, cNo).(*lproto.Error)
	if !ok {
		t.Fatal("unauthenticated subscribe should yield an Error frame")
	}
	if errFrame.Code != string(lproto.CodeAuthDenied) {
		t.Fatalf("error code = %q, want AUTH_DENIED", errFrame.Code)
	}
}
