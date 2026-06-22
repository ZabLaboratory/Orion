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

// mountWire serves the kit LSDP handler on an httptest server, rewriting
// the path to /lsdp.v1 like the public router does, and returns the ws
// URL. Shared by the switch-flow tests.
func mountWire(t *testing.T, wire *Wire) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/lsdp.v1"
		wire.Handler().ServeHTTP(w, r2)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func passthroughGraph(id, version, path string) *compiler.Graph {
	return &compiler.Graph{
		SceneID:      id,
		SceneVersion: version,
		Nodes: []compiler.GraphNode{
			{ID: "in." + path, Kind: "input"},
			{ID: "out." + path, Kind: "output", Path: path, Compute: "core.passthrough", Upstream: []string{"in." + path}},
		},
		Defaults: map[string]json.RawMessage{
			path: json.RawMessage(`0`),
		},
	}
}

// TestLSDP_LateJoinAfterSceneSwitchGetsKeyframe is the full Pulsar prod
// flow: a show starts on scene A, the operator switches to scene B (the
// leaderboard), the board is computed reactively on B, and ONLY THEN
// does Pulsar connect. The late LSDP join must receive a keyframe for
// the ACTIVE scene (B) with B's scene_version and B's current state —
// not A, not empty.
//
// This exercises the kit's "first registered scene wins as active"
// (NewScene) interacting with a later Show.SetActive: the join must
// resolve the active scene to B and ship B's board.
func TestLSDP_LateJoinAfterSceneSwitchGetsKeyframe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := quietLogger(t)
	wire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	show := runtime.NewShow(runtime.NewComputeRegistry(), logger)
	show.SetMirrors(wire)
	t.Cleanup(show.Stop)

	// Scene A loads and activates first — this makes A the kit's active
	// scene via NewScene's first-wins rule.
	show.Load("scene-a", passthroughGraph("scene-a", "sha256:ver-a", "score.team_a"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-a"})
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatalf("SetActive A: %v", err)
	}

	// Scene B (the leaderboard) loads, then the operator switches to it.
	show.Load("scene-b", passthroughGraph("scene-b", "sha256:ver-b", "board.row0"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-b"})
	if err := show.SetActive("scene-b", nil); err != nil {
		t.Fatalf("SetActive B: %v", err)
	}
	sceneB, err := show.Get("scene-b")
	if err != nil {
		t.Fatalf("Get scene-b: %v", err)
	}

	// Board computed reactively on B, AFTER the switch, with no LSDP
	// subscriber attached (Pulsar still not connected).
	if !sceneB.Input(runtime.InputMsg{
		Path:  "board.row0",
		Value: json.RawMessage(`"1. GIDEON"`),
	}) {
		t.Fatal("scene-b inbox full")
	}
	waitForState(t, wire, sceneB, "board.row0", `"1. GIDEON"`)

	// Pulsar finally joins. Live mode (no scene field) → must resolve to
	// the ACTIVE scene B and ship B's keyframe.
	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")

	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatalf("late join after switch: first frame must be a snapshot keyframe")
	}
	if snap.SceneID != "scene-b" {
		t.Fatalf("keyframe scene_id = %q, want scene-b (active)", snap.SceneID)
	}
	// @lumencast/runtime rejects a join snapshot with seq < 1 (VERSION_GAP
	// → onError → black screen). Even after a scene switch the destination
	// keyframe MUST carry seq >= 1.
	if snap.Seq < 1 {
		t.Fatalf("keyframe seq = %d, want >= 1 (runtime rejects seq<1 as VERSION_GAP)", snap.Seq)
	}
	if snap.SceneVersion != "sha256:ver-b" {
		t.Fatalf("keyframe scene_version = %q, want sha256:ver-b", snap.SceneVersion)
	}
	if string(snap.State["board.row0"]) != `"1. GIDEON"` {
		t.Fatalf("keyframe state[board.row0] = %q, want \"1. GIDEON\" (board lost)", snap.State["board.row0"])
	}
}
