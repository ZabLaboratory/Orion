package lsdp

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	lproto "github.com/Lumencast/lumencast-go/protocol"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func boolPtr(b bool) *bool { return &b }

// readUntilLeaves reads server frames until every requested leaf has been
// observed (in a Snapshot's State or any Delta's Patches), then returns their
// values. Unlike readUntilSlot it accumulates across a SINGLE multi-patch delta
// — the overlay mirror emits running + on_air together, so a per-leaf read would
// consume the whole delta on the first lookup and starve the second.
func readUntilLeaves(ctx context.Context, t *testing.T, c *websocket.Conn, leaves ...string) map[string]string {
	t.Helper()
	want := make(map[string]struct{}, len(leaves))
	for _, l := range leaves {
		want[l] = struct{}{}
	}
	got := make(map[string]string, len(leaves))
	for i := 0; i < 8 && len(got) < len(want); i++ {
		switch m := readServerFrame(ctx, t, c).(type) {
		case *lproto.Snapshot:
			for l := range want {
				if v, ok := m.State[l]; ok {
					got[l] = string(v)
				}
			}
		case *lproto.Delta:
			for _, p := range m.Patches {
				if _, ok := want[p.Path]; ok {
					got[p.Path] = string(p.Value)
				}
			}
		}
	}
	if len(got) < len(want) {
		t.Fatalf("leaves %v never all reached the wire (got %v)", leaves, got)
	}
	return got
}

// TestLSDP_OverlayAppEmitsDelta (issue #283): a stream-level overlay-app.set
// emits the reserved `__overlay.<app_id>.running` / `.on_air` boolean leaves on
// the active wire — the control state Prism (#360) reconciles the app + its
// window_capture item from, without a scene switch or re-push.
func TestLSDP_OverlayAppEmitsDelta(t *testing.T) {
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

	show.Load("scene-a", passthroughGraph("scene-a", "sha256:ver-a", "score.team_a"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-a"})
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatalf("SetActive A: %v", err)
	}

	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	// The op (post-fire) sets running=true, on_air=false — both leaves ride a
	// single delta.
	wire.EmitOverlayApp("app-1", boolPtr(true), boolPtr(false))

	got := readUntilLeaves(ctx, t, c, "__overlay.app-1.running", "__overlay.app-1.on_air")
	if got["__overlay.app-1.running"] != `true` {
		t.Fatalf("running leaf = %s, want true", got["__overlay.app-1.running"])
	}
	if got["__overlay.app-1.on_air"] != `false` {
		t.Fatalf("on_air leaf = %s, want false", got["__overlay.app-1.on_air"])
	}
}

// TestLSDP_OverlayAppPartialUpdate: a set carrying only `on_air` emits ONLY the
// on_air leaf — the untouched dimension is not written (partial update).
func TestLSDP_OverlayAppPartialUpdate(t *testing.T) {
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

	show.Load("scene-a", passthroughGraph("scene-a", "sha256:ver-a", "score.team_a"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-a"})
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatalf("SetActive A: %v", err)
	}

	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	// running left nil → only the on_air leaf is emitted.
	wire.EmitOverlayApp("app-1", nil, boolPtr(true))
	if got := readUntilSlot(ctx, t, c, "__overlay.app-1.on_air"); got != `true` {
		t.Fatalf("on_air leaf = %s, want true", got)
	}
}

// TestLSDP_OverlayAppPersistsAcrossSceneSwitch: the overlay control state is
// stream-level — it survives a switch of active scene. A viewer joining the NEW
// active scene after the switch gets the leaves in its keyframe, even though no
// scene binds `__overlay.*` (it rides the wire above the per-scene bound-leaf
// gate).
func TestLSDP_OverlayAppPersistsAcrossSceneSwitch(t *testing.T) {
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

	show.Load("scene-a", passthroughGraph("scene-a", "sha256:ver-a", "score.team_a"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-a"})
	show.Load("scene-b", passthroughGraph("scene-b", "sha256:ver-b", "score.team_b"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-b"})
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatalf("SetActive A: %v", err)
	}

	// Set the control state on scene-a, then switch to scene-b.
	wire.EmitOverlayApp("app-1", boolPtr(true), boolPtr(true))
	if err := show.SetActive("scene-b", nil); err != nil {
		t.Fatalf("SetActive B: %v", err)
	}

	// A viewer joining AFTER the switch (now on scene-b) must see the overlay
	// leaves in its keyframe snapshot — replayed at SetActive.
	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if got := readUntilSlot(ctx, t, c, "__overlay.app-1.running"); got != `true` {
		t.Fatalf("running leaf after switch = %s, want true (stream-level persist)", got)
	}
}
