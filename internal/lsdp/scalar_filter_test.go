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

	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// boardScene mirrors a real M3 leaderboard scene's reactive state: a
// renderable scalar board plus the db.query intermediates (rows, clause
// descriptors) that the engine keeps as object/array leaves. The wire
// must emit ONLY the scalars; the objects/arrays-of-object would make
// @lumencast/protocol reject the entire snapshot (INVALID_VALUE →
// reconnect loop → the prod black screen).
//
// This drives Forward directly (the filter seam) rather than through a
// compute, so the non-scalar intermediates can be injected verbatim.
func boardSceneState() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		// Renderable scalars — MUST survive the filter.
		"__vars..leaderboard_display": json.RawMessage(`"1. GIDEON — 9\n2. AATROX — 7"`),
		"catA0":                       json.RawMessage(`"1. GIDEON"`),
		"catA1":                       json.RawMessage(`"2. AATROX"`),
		"chat.display":                json.RawMessage(`"hello chat"`),
		"score.team_a":                json.RawMessage(`9`),
		"flags.live":                  json.RawMessage(`true`),
		"nothing":                     json.RawMessage(`null`),
		// Scalar array is wire-legal (point/dimension encoding, §3.2.1).
		"layout.origin": json.RawMessage(`[12,34]`),

		// db.query intermediates — objects and arrays-of-object. MUST be
		// dropped (forbidden in patch values, §3.2.1).
		"__vars..ranking_rows": json.RawMessage(`[{"summoner_name":"GIDEON"},{"summoner_name":"AATROX"}]`),
		"__vars..row_0":        json.RawMessage(`[{"summoner_name":"GIDEON"}]`),
		"__vars..row_1":        json.RawMessage(`[{"summoner_name":"AATROX"}]`),
		"clause0":              json.RawMessage(`{"op":"eq","field":"split_id"}`),
		"clauseLit0":           json.RawMessage(`{"v":42}`),
	}
}

var wantScalarPaths = []string{
	"__vars..leaderboard_display", "catA0", "catA1",
	"chat.display", "score.team_a", "flags.live", "nothing", "layout.origin",
}

var wantDroppedPaths = []string{
	"__vars..ranking_rows", "__vars..row_0", "__vars..row_1",
	"clause0", "clauseLit0",
}

func assertOnlyScalars(t *testing.T, label string, got map[string]string) {
	t.Helper()
	for _, p := range wantScalarPaths {
		if _, ok := got[p]; !ok {
			t.Errorf("%s: scalar leaf %q was dropped — board would be lost", label, p)
		}
	}
	for _, p := range wantDroppedPaths {
		if v, ok := got[p]; ok {
			t.Errorf("%s: non-scalar leaf %q leaked to the wire as %q — Solar would reject the whole frame", label, p, v)
		}
	}
}

// TestLSDP_SnapshotEmitsOnlyScalars is the prod root-cause regression:
// the snapshot pushed through the mirror tap must drop every object /
// array-of-object intermediate, keeping only renderable scalars. The
// kit store (what a late joiner's keyframe carries) must hold only the
// scalars.
func TestLSDP_SnapshotEmitsOnlyScalars(t *testing.T) {
	wire, err := NewWire(quietLogger(t))
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorFor("scene-1", "sha256:test-1").(*sceneMirror)

	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State:        boardSceneState(),
	})

	assertOnlyScalars(t, "snapshot", wire.kitState("scene-1"))

	// The board scalar specifically must be present and intact.
	if got := wire.kitState("scene-1")["__vars..leaderboard_display"]; got != `"1. GIDEON — 9\n2. AATROX — 7"` {
		t.Fatalf("leaderboard_display = %q, want the rendered board scalar", got)
	}
}

// TestLSDP_DeltaEmitsOnlyScalars proves the same filter on the delta
// path: a delta that updates row_k (an array-of-object intermediate)
// alongside the board scalar must emit only the scalar.
func TestLSDP_DeltaEmitsOnlyScalars(t *testing.T) {
	wire, err := NewWire(quietLogger(t))
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorFor("scene-1", "sha256:test-1").(*sceneMirror)

	// Seed a baseline scalar so the scene store is non-empty.
	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State: map[string]json.RawMessage{
			"__vars..leaderboard_display": json.RawMessage(`"old"`),
		},
	})

	m.Forward(&protocol.Delta{
		SceneID: "scene-1",
		Patches: []protocol.Patch{
			{Path: "__vars..leaderboard_display", Value: json.RawMessage(`"1. GIDEON — 9"`)},
			{Path: "catA0", Value: json.RawMessage(`"1. GIDEON"`)},
			{Path: "__vars..row_0", Value: json.RawMessage(`[{"summoner_name":"GIDEON"}]`)},
			{Path: "clause0", Value: json.RawMessage(`{"op":"eq"}`)},
		},
	})

	got := wire.kitState("scene-1")
	if got["__vars..leaderboard_display"] != `"1. GIDEON — 9"` {
		t.Errorf("delta did not update board scalar: %q", got["__vars..leaderboard_display"])
	}
	if got["catA0"] != `"1. GIDEON"` {
		t.Errorf("delta dropped scalar catA0: %q", got["catA0"])
	}
	if v, ok := got["__vars..row_0"]; ok {
		t.Errorf("delta leaked array-of-object row_0 as %q", v)
	}
	if v, ok := got["clause0"]; ok {
		t.Errorf("delta leaked object clause0 as %q", v)
	}
}

// TestLSDP_AllNonScalarDeltaIsDropped: a delta carrying ONLY
// intermediates must not reach Emit (which rejects empty maps) and must
// not error — it is silently dropped.
func TestLSDP_AllNonScalarDeltaIsDropped(t *testing.T) {
	wire, err := NewWire(quietLogger(t))
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorFor("scene-1", "sha256:test-1").(*sceneMirror)
	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State:        map[string]json.RawMessage{"catA0": json.RawMessage(`"x"`)},
	})

	// Should be a no-op: nothing scalar to emit.
	m.Forward(&protocol.Delta{
		SceneID: "scene-1",
		Patches: []protocol.Patch{
			{Path: "clause0", Value: json.RawMessage(`{"op":"eq"}`)},
			{Path: "__vars..row_0", Value: json.RawMessage(`[{"summoner_name":"GIDEON"}]`)},
		},
	})

	if v, ok := wire.kitState("scene-1")["clause0"]; ok {
		t.Errorf("intermediate-only delta leaked clause0 as %q", v)
	}
}

// TestLSDP_RealDecoderAcceptsFilteredSnapshot is the end-to-end proof
// against the kit's own decoder (same recursive §3.2.1 rule as
// @lumencast/protocol): an LSDP subscriber joining a scene whose store
// was fed object/array intermediates receives a keyframe that decodes
// WITHOUT rejection, and carries the board scalar. This is the exact
// path that was failing in prod (decode reject → reconnect → black).
func TestLSDP_RealDecoderAcceptsFilteredSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, err := NewWire(quietLogger(t))
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorFor("scene-1", "sha256:test-1").(*sceneMirror)
	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State:        boardSceneState(),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/lsdp.v1"
		wire.Handler().ServeHTTP(w, r2)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	c := dialLSDP(ctx, t, wsURL, "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")

	// readServerFrame calls lproto.DecodeServer, which enforces §3.2.1.
	// Before the fix this fataled with INVALID_VALUE on the first object
	// leaf. After the fix the keyframe decodes cleanly.
	frame := readServerFrame(ctx, t, c)
	snap, ok := frame.(*lproto.Snapshot)
	if !ok {
		t.Fatalf("first frame %T, want decoded snapshot keyframe", frame)
	}
	if snap.SceneID != "scene-1" || snap.SceneVersion != "sha256:test-1" {
		t.Fatalf("keyframe id/version = %q/%q", snap.SceneID, snap.SceneVersion)
	}
	if string(snap.State["__vars..leaderboard_display"]) != `"1. GIDEON — 9\n2. AATROX — 7"` {
		t.Fatalf("keyframe missing board scalar: %v", snap.State["__vars..leaderboard_display"])
	}
	for _, p := range wantDroppedPaths {
		if _, present := snap.State[p]; present {
			t.Fatalf("keyframe carried forbidden intermediate %q — decoder would reject in prod", p)
		}
	}
}
