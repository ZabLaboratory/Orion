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
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// m3Bundle is the renderable surface of the prod M3 leaderboard scene:
// a text node bound to the board scalar and a text node bound to the
// chat scalar. Nothing else is bound — every `__vars..` compute leaf,
// the empty WHERE literals, and the scalar work leaves are internal.
func m3Bundle() *compiler.RenderBundle {
	return &compiler.RenderBundle{
		SceneVersion: "sha256:test-1",
		Root: compiler.LayoutNode{
			Kind: "stack",
			Children: []compiler.LayoutNode{
				{Kind: "text", ID: "board", Bindings: map[string]string{"value": "__vars..leaderboard_display"}},
				{Kind: "text", ID: "chat", Bindings: map[string]string{"value": "chat.display"}},
			},
		},
	}
}

// m3SceneStateWithInternals is the FULL reactive store of the prod M3
// scene as it actually exists post-#132: the two renderable scalars PLUS
// the internal compute that #132's shape filter could not catch — the
// five empty WHERE literals `whereEmptyN=[]` (array-of-scalar, legal by
// shape) and the scalar work leaves `catA0`/`getScore0` (scalars, legal
// by shape) — and the object/array-of-object intermediates #132 already
// drops by shape. Only the two bound scalars must survive the gate.
func m3SceneStateWithInternals() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		// Renderable (bound) — MUST survive.
		"__vars..leaderboard_display": json.RawMessage(`"1. GIDEON — 9\n2. AATROX — 7"`),
		"chat.display":                json.RawMessage(`"hello chat"`),

		// Scalar WORK leaves — legal by shape, but bound by NOTHING.
		// #132 let these through; the bound gate drops them.
		"catA0":     json.RawMessage(`"1. GIDEON"`),
		"catA1":     json.RawMessage(`"2. AATROX"`),
		"getScore0": json.RawMessage(`9`),

		// The five empty WHERE literals — array-of-scalar (legal by
		// shape, the exact case missing from scalar_filter_test.go).
		// Bound by nothing → dropped.
		"whereEmpty0": json.RawMessage(`[]`),
		"whereEmpty1": json.RawMessage(`[]`),
		"whereEmpty2": json.RawMessage(`[]`),
		"whereEmpty3": json.RawMessage(`[]`),
		"whereEmpty4": json.RawMessage(`[]`),

		// Object / array-of-object intermediates — also dropped (the
		// shape filter would catch these too; the gate catches them first).
		"__vars..ranking_rows": json.RawMessage(`[{"summoner_name":"GIDEON"}]`),
		"clause0":              json.RawMessage(`{"op":"eq","field":"split_id"}`),
	}
}

var m3WantKept = []string{"__vars..leaderboard_display", "chat.display"}

var m3WantDropped = []string{
	"catA0", "catA1", "getScore0",
	"whereEmpty0", "whereEmpty1", "whereEmpty2", "whereEmpty3", "whereEmpty4",
	"__vars..ranking_rows", "clause0",
}

// TestBoundLeaves_SnapshotEmitsOnlyBound is the definitive regression:
// the bound-leaf gate keeps ONLY the leaves the layout binds. Every
// compute intermediate — including the array-of-scalar `whereEmptyN=[]`
// that slipped through #132's shape filter and the scalar work leaves
// `catA0`/`getScore0` — is dropped at the tap.
func TestBoundLeaves_SnapshotEmitsOnlyBound(t *testing.T) {
	wire, err := NewWire(quietLogger(t))
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorFor("scene-1", "sha256:test-1", m3Bundle()).(*sceneMirror)

	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State:        m3SceneStateWithInternals(),
	})

	got := wire.kitState("scene-1")
	for _, p := range m3WantKept {
		if _, ok := got[p]; !ok {
			t.Errorf("bound leaf %q was dropped — board/chat would be lost", p)
		}
	}
	for _, p := range m3WantDropped {
		if v, ok := got[p]; ok {
			t.Errorf("unbound intermediate %q leaked to the wire as %q", p, v)
		}
	}
	if got["__vars..leaderboard_display"] != `"1. GIDEON — 9\n2. AATROX — 7"` {
		t.Fatalf("board scalar corrupted: %q", got["__vars..leaderboard_display"])
	}
}

// TestBoundLeaves_WhereEmptyArrayRegression pins the exact gap Keeper
// flagged: `whereEmptyN=[]` is array-of-scalar, so isLSDPScalar accepts
// it (legal §3.2.1 shape) — yet it is an internal literal bound by no
// node. The bound gate must drop it. (This is the case that was missing
// from scalar_filter_test.go.)
func TestBoundLeaves_WhereEmptyArrayRegression(t *testing.T) {
	bundle := m3Bundle()
	bound := boundLeavesFromBundle(bundle)

	for i := 0; i < 5; i++ {
		path := "whereEmpty" + string(rune('0'+i))
		// Shape filter ACCEPTS it (the #132 gap)…
		if !isLSDPScalar(json.RawMessage(`[]`)) {
			t.Fatalf("precondition: isLSDPScalar should accept [] (array-of-scalar)")
		}
		// …but the bound gate REJECTS it (not bound, not a descendant).
		if bound.renderable(path) {
			t.Errorf("%q is not bound; bound gate must reject it", path)
		}
	}
	// The two real bindings are renderable.
	for _, p := range m3WantKept {
		if !bound.renderable(p) {
			t.Errorf("bound leaf %q must be renderable", p)
		}
	}
}

// TestBoundLeaves_RepeatItemDescendantsSurvive proves the prefix
// semantics: a `repeat` node binding `items: "rows"` makes `rows`
// renderable, AND every per-iteration descendant `rows.{i}.{field}`
// (resolved at render time by the runtime's path scope) survives the
// gate. An exact-match allow-list would drop these and black out the
// list — this asserts we do not.
func TestBoundLeaves_RepeatItemDescendantsSurvive(t *testing.T) {
	bundle := &compiler.RenderBundle{
		SceneVersion: "sha256:rep",
		Root: compiler.LayoutNode{
			Kind: "repeat",
			ID:   "rows",
			Bindings: map[string]string{"items": "rows"},
			Children: []compiler.LayoutNode{
				{Kind: "text", Bindings: map[string]string{"value": "name"}},
			},
		},
	}
	bound := boundLeavesFromBundle(bundle)

	for _, p := range []string{"rows", "rows.0.name", "rows.1.name", "rows.42.score"} {
		if !bound.renderable(p) {
			t.Errorf("repeat descendant %q must survive the gate", p)
		}
	}
	// A sibling internal leaf that merely shares a prefix substring (but
	// not a dot-boundary ancestor) must NOT be smuggled through.
	if bound.renderable("rowsInternal") {
		t.Errorf("`rowsInternal` is not a dot-descendant of `rows` and must be dropped")
	}
}

// TestBoundLeaves_DisabledWhenNoBindings proves fail-open: a bundle with
// no bindings disables the bound gate, so the scalar filter alone gates
// the wire (a passthrough/operator-only scene is never blacked out).
func TestBoundLeaves_DisabledWhenNoBindings(t *testing.T) {
	bound := boundLeavesFromBundle(&compiler.RenderBundle{SceneVersion: "x"})
	if bound.active() {
		t.Fatalf("a binding-less bundle must yield a disabled (inactive) gate")
	}
	if boundLeavesFromBundle(nil).active() {
		t.Fatalf("a nil bundle must yield a disabled (inactive) gate")
	}

	// Through the mirror: with no bundle, a scalar work leaf still passes
	// (scalar filter only), exactly as before this change.
	wire, err := NewWire(quietLogger(t))
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorFor("scene-1", "sha256:test-1", nil).(*sceneMirror)
	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State: map[string]json.RawMessage{
			"catA0":       json.RawMessage(`"x"`),
			"whereEmpty0": json.RawMessage(`[]`),
			"clause0":     json.RawMessage(`{"op":"eq"}`), // object: shape filter drops
		},
	})
	got := wire.kitState("scene-1")
	if _, ok := got["catA0"]; !ok {
		t.Errorf("gate disabled: scalar leaf catA0 must pass the scalar filter")
	}
	if _, ok := got["clause0"]; ok {
		t.Errorf("gate disabled: object clause0 must still be dropped by the scalar filter")
	}
}

// TestBoundLeaves_DeltaEmitsOnlyBound proves the same gate on the delta
// path: a delta touching the bound board scalar alongside an unbound
// empty-array literal emits only the board scalar.
func TestBoundLeaves_DeltaEmitsOnlyBound(t *testing.T) {
	wire, err := NewWire(quietLogger(t))
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorFor("scene-1", "sha256:test-1", m3Bundle()).(*sceneMirror)
	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State:        map[string]json.RawMessage{"__vars..leaderboard_display": json.RawMessage(`"old"`)},
	})

	m.Forward(&protocol.Delta{
		SceneID: "scene-1",
		Patches: []protocol.Patch{
			{Path: "__vars..leaderboard_display", Value: json.RawMessage(`"1. GIDEON — 9"`)},
			{Path: "whereEmpty0", Value: json.RawMessage(`[]`)},
			{Path: "getScore0", Value: json.RawMessage(`9`)},
		},
	})

	got := wire.kitState("scene-1")
	if got["__vars..leaderboard_display"] != `"1. GIDEON — 9"` {
		t.Errorf("delta did not update bound board scalar: %q", got["__vars..leaderboard_display"])
	}
	if _, ok := got["whereEmpty0"]; ok {
		t.Errorf("delta leaked unbound empty-array whereEmpty0")
	}
	if _, ok := got["getScore0"]; ok {
		t.Errorf("delta leaked unbound scalar work leaf getScore0")
	}
}

// TestBoundLeaves_RealDecoderRendersBoard is the end-to-end proof: an
// LSDP subscriber joining the gated scene receives a keyframe that
// decodes against @lumencast/protocol AND carries ONLY the bound board +
// chat scalars (no `whereEmptyN`, no work leaves), so the renderable
// surface is exactly the board — the definitive fix for the prod symptom.
func TestBoundLeaves_RealDecoderRendersBoard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, err := NewWire(quietLogger(t))
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorFor("scene-1", "sha256:test-1", m3Bundle()).(*sceneMirror)
	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State:        m3SceneStateWithInternals(),
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

	frame := readServerFrame(ctx, t, c)
	snap, ok := frame.(*lproto.Snapshot)
	if !ok {
		t.Fatalf("first frame %T, want decoded snapshot keyframe", frame)
	}
	if string(snap.State["__vars..leaderboard_display"]) != `"1. GIDEON — 9\n2. AATROX — 7"` {
		t.Fatalf("keyframe missing board scalar: %v", snap.State["__vars..leaderboard_display"])
	}
	if string(snap.State["chat.display"]) != `"hello chat"` {
		t.Fatalf("keyframe missing chat scalar: %v", snap.State["chat.display"])
	}
	for _, p := range m3WantDropped {
		if _, present := snap.State[p]; present {
			t.Fatalf("keyframe carried unbound intermediate %q — surface not clean", p)
		}
	}
}
