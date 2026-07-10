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

// readUntilOverlayApps reads server frames until an overlay_apps frame arrives
// (skipping the join snapshot / roster / deltas), then returns it.
func readUntilOverlayApps(ctx context.Context, t *testing.T, c *websocket.Conn) *lproto.OverlayApps {
	t.Helper()
	for i := 0; i < 8; i++ {
		if m, ok := readServerFrameRaw(ctx, t, c).(*lproto.OverlayApps); ok {
			return m
		}
	}
	t.Fatal("overlay_apps frame never arrived")
	return nil
}

// TestLSDP_OverlayAppEmitsFrame (issue #283, channel #292): a stream-level
// overlay-app.set publishes the show-level `overlay_apps` frame carrying the
// desired {running, on_air} — the control state Prism (#360) reconciles the
// app + its window_capture item from, without a scene switch or re-push.
func TestLSDP_OverlayAppEmitsFrame(t *testing.T) {
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

	wire.EmitOverlayApp("app-1", boolPtr(true), boolPtr(false))

	frame := readUntilOverlayApps(ctx, t, c)
	app, ok := frame.Apps["app-1"]
	if !ok {
		t.Fatalf("overlay_apps missing app-1: %+v", frame.Apps)
	}
	if app.Running == nil || !*app.Running {
		t.Fatalf("running = %v, want true", app.Running)
	}
	if app.OnAir == nil || *app.OnAir {
		t.Fatalf("on_air = %v, want false", app.OnAir)
	}
}

// TestLSDP_OverlayAppPartialUpdate: a set carrying only `on_air` yields a frame
// whose app has OnAir set and Running absent (nil) — partial update preserved.
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

	wire.EmitOverlayApp("app-1", nil, boolPtr(true))

	frame := readUntilOverlayApps(ctx, t, c)
	app := frame.Apps["app-1"]
	if app.OnAir == nil || !*app.OnAir {
		t.Fatalf("on_air = %v, want true", app.OnAir)
	}
	if app.Running != nil {
		t.Fatalf("running must stay absent (nil), got %v", *app.Running)
	}
}

// TestLSDP_OverlayAppDeliveredWithoutActiveScene (issue #292): the show-level
// channel makes the overlay control state deliverable even with NO active scene
// (the Marker case). The mirror emits before any scene is loaded; a viewer that
// joins the scene-less show still receives the overlay_apps frame (kit cache +
// holding-scene replay). This is what the old scene-riding leaves could NOT do.
func TestLSDP_OverlayAppDeliveredWithoutActiveScene(t *testing.T) {
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

	// NO Load, NO SetActive — an empty show. The overlay set still publishes.
	wire.EmitOverlayApp("app-1", boolPtr(true), boolPtr(true))

	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")

	frame := readUntilOverlayApps(ctx, t, c)
	app, ok := frame.Apps["app-1"]
	if !ok {
		t.Fatalf("scene-less join did not receive app-1 overlay state: %+v", frame.Apps)
	}
	if app.Running == nil || !*app.Running || app.OnAir == nil || !*app.OnAir {
		t.Fatalf("app-1 state wrong: running=%v on_air=%v", app.Running, app.OnAir)
	}
}

// TestLSDP_OverlayAppSurvivesSceneSwitch: the state is show-level, so a viewer
// joining AFTER a scene switch still receives it — now from the kit cache, no
// per-SetActive replay needed.
func TestLSDP_OverlayAppSurvivesSceneSwitch(t *testing.T) {
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
	wire.EmitOverlayApp("app-1", boolPtr(true), boolPtr(true))
	if err := show.SetActive("scene-b", nil); err != nil {
		t.Fatalf("SetActive B: %v", err)
	}

	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")

	frame := readUntilOverlayApps(ctx, t, c)
	if app, ok := frame.Apps["app-1"]; !ok || app.Running == nil || !*app.Running {
		t.Fatalf("overlay state lost across switch: %+v", frame.Apps)
	}
}
