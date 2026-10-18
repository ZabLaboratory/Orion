package server_test

import (
	"testing"

	"github.com/Lumencast/lumencast-go/protocol"
)

func boolPtr(b bool) *bool { return &b }

func appsFixture() map[string]protocol.OverlayAppState {
	return map[string]protocol.OverlayAppState{
		"cam": {Running: boolPtr(true), OnAir: boolPtr(false)},
		"lt":  {Running: boolPtr(true)}, // partial: only the running dimension
	}
}

// TestOverlayApps_ReplayedAfterSnapshot: a fresh 1.1 live subscriber receives
// the cached overlay-app state right after its snapshot (mirror of the roster).
func TestOverlayApps_ReplayedAfterSnapshot(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	scene := srv.NewScene("main")
	if err := scene.Set(map[string]any{"show.title": "Hello"}); err != nil {
		t.Fatal(err)
	}
	srv.SetOverlayApps(appsFixture())

	c := dialV11(t, url)
	send(t, c, &protocol.Subscribe{Token: "viewer-tok"})

	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatalf("first frame is not snapshot")
	}
	overlay, ok := recv(t, c).(*protocol.OverlayApps)
	if !ok {
		t.Fatalf("second frame is not overlay_apps")
	}
	assertApps(t, overlay)
}

// TestOverlayApps_DeliverableWithNoActiveScene is THE blocking criterion: with
// NO scene ever registered (an empty show, empty roster), a live 1.1 subscriber
// can still connect and receives the overlay_apps frame. This is the Marker
// case — an overlay consumer with no active scene.
func TestOverlayApps_DeliverableWithNoActiveScene(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	srv.SetOverlayApps(appsFixture())

	c := dialV11(t, url) // no NewScene, no SetActive — the show is empty
	send(t, c, &protocol.Subscribe{Token: "viewer-tok"})

	// The scene-less join still yields a (holding) snapshot, then the overlay.
	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatalf("scene-less join: first frame is not snapshot")
	}
	overlay, ok := recv(t, c).(*protocol.OverlayApps)
	if !ok {
		t.Fatalf("scene-less join did NOT receive overlay_apps")
	}
	assertApps(t, overlay)
}

// TestOverlayApps_FanoutReachesSceneLessSubscriber: a SetOverlayApps AFTER a
// scene-less subscriber has joined still fans out to it (the holding scene is
// part of the show-level fan-out set).
func TestOverlayApps_FanoutReachesSceneLessSubscriber(t *testing.T) {
	srv, url := startTestServer(t, opAuth())

	c := dialV11(t, url) // empty show
	send(t, c, &protocol.Subscribe{Token: "viewer-tok"})
	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatalf("first frame is not snapshot")
	}

	// State published AFTER the join reaches the holding subscriber.
	srv.SetOverlayApps(appsFixture())
	overlay, ok := recv(t, c).(*protocol.OverlayApps)
	if !ok {
		t.Fatalf("scene-less subscriber did not receive fanned-out overlay_apps")
	}
	assertApps(t, overlay)
}

// TestOverlayApps_SceneLessSubscriberMigratesOnActivation: a subscriber that
// joined with no scene is migrated onto the first scene that becomes active,
// receiving SceneChanged + a fresh snapshot — and keeps receiving overlay.
func TestOverlayApps_SceneLessSubscriberMigratesOnActivation(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	srv.SetOverlayApps(appsFixture())

	c := dialV11(t, url) // empty show
	send(t, c, &protocol.Subscribe{Token: "viewer-tok"})
	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatalf("holding snapshot expected")
	}
	if _, ok := recv(t, c).(*protocol.OverlayApps); !ok {
		t.Fatalf("overlay replay expected")
	}

	// First scene appears → the holding subscriber migrates onto it.
	scene := srv.NewScene("main")
	if err := scene.Set(map[string]any{"show.title": "Live"}); err != nil {
		t.Fatal(err)
	}
	sc, ok := recv(t, c).(*protocol.SceneChanged)
	if !ok {
		t.Fatalf("expected SceneChanged on activation, got %T", sc)
	}
	if sc.SceneID != "main" {
		t.Fatalf("SceneChanged to %q, want main", sc.SceneID)
	}
	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatalf("expected fresh snapshot after migration")
	}
}

// TestOverlayApps_NotSentToLegacy10Subscribers: a 1.0 connection never receives
// the additive overlay_apps frame (1.1-only surface, parity with scene_roster).
func TestOverlayApps_NotSentToLegacy10Subscribers(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	scene := srv.NewScene("main")
	if err := scene.Set(map[string]any{"show.title": "Hello"}); err != nil {
		t.Fatal(err)
	}
	srv.SetOverlayApps(appsFixture())

	c := dial(t, url) // 1.0 subprotocol
	send(t, c, &protocol.Subscribe{Token: "viewer-tok"})
	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatalf("first frame is not snapshot")
	}
	// A live update must arrive next — no overlay_apps wedged in.
	if err := scene.Emit(map[string]any{"show.title": "World"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := recv(t, c).(*protocol.Delta); !ok {
		t.Fatalf("1.0 subscriber received a non-delta frame (leaked overlay_apps?)")
	}
}

func assertApps(t *testing.T, overlay *protocol.OverlayApps) {
	t.Helper()
	if len(overlay.Apps) != 2 {
		t.Fatalf("overlay apps = %+v, want 2 entries", overlay.Apps)
	}
	cam, ok := overlay.Apps["cam"]
	if !ok || cam.Running == nil || !*cam.Running || cam.OnAir == nil || *cam.OnAir {
		t.Fatalf("cam state wrong: %+v", cam)
	}
	lt, ok := overlay.Apps["lt"]
	if !ok || lt.Running == nil || !*lt.Running || lt.OnAir != nil {
		t.Fatalf("lt partial state wrong (on_air should be absent): %+v", lt)
	}
}
