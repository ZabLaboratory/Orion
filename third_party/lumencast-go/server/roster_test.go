package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Lumencast/lumencast-go/protocol"
)

// dialV11 opens a connection advertising the lsdp.v1.1 subprotocol so
// the server tags the subscription as 1.1-capable (roster-eligible).
func dialV11(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{protocol.SubProtocolV1_1},
		HTTPClient:   &http.Client{Timeout: 2 * time.Second},
	})
	if err != nil {
		t.Fatalf("dial v1.1: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

// TestRoster_ReplayedAfterSnapshot : a fresh 1.1 live subscriber
// receives the cached roster immediately after its initial snapshot.
func TestRoster_ReplayedAfterSnapshot(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	scene := srv.NewScene("main")
	if err := scene.Set(map[string]any{"show.title": "Hello"}); err != nil {
		t.Fatal(err)
	}
	srv.SetRoster([]protocol.RosterEntry{
		{SceneID: "main", SceneVersion: "sha256:aaa"},
		{SceneID: "brb", SceneVersion: "sha256:bbb"},
	})

	c := dialV11(t, url)
	send(t, c, &protocol.Subscribe{Token: "viewer-tok"})

	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatalf("first frame is not snapshot")
	}
	roster, ok := recv(t, c).(*protocol.SceneRoster)
	if !ok {
		t.Fatalf("second frame is not scene_roster")
	}
	if len(roster.Entries) != 2 || roster.Entries[0].SceneID != "main" ||
		roster.Entries[1].SceneVersion != "sha256:bbb" {
		t.Fatalf("unexpected roster entries: %+v", roster.Entries)
	}
}

// TestRoster_NoReplayBeforeSet : with no roster ever set, a subscriber
// gets its snapshot and then live frames — never a spurious roster.
func TestRoster_NoReplayBeforeSet(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	scene := srv.NewScene("main")
	if err := scene.Set(map[string]any{"show.title": "Hello"}); err != nil {
		t.Fatal(err)
	}

	c := dialV11(t, url)
	send(t, c, &protocol.Subscribe{Token: "viewer-tok"})
	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatalf("first frame is not snapshot")
	}

	// Emit a delta ; the very next frame must be that delta, proving no
	// roster was injected after the snapshot.
	if err := scene.Emit(map[string]any{"show.title": "World"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := recv(t, c).(*protocol.Delta); !ok {
		t.Fatalf("expected delta, got a different frame (spurious roster?)")
	}
}

// TestRoster_FannedOutToAllLiveSubs : SetRoster reaches every live 1.1
// subscriber already attached.
func TestRoster_FannedOutToAllLiveSubs(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	scene := srv.NewScene("main")
	if err := scene.Set(map[string]any{"show.title": "Hello"}); err != nil {
		t.Fatal(err)
	}

	c1 := dialV11(t, url)
	send(t, c1, &protocol.Subscribe{Token: "viewer-tok"})
	if _, ok := recv(t, c1).(*protocol.Snapshot); !ok {
		t.Fatalf("c1 first frame is not snapshot")
	}
	c2 := dialV11(t, url)
	send(t, c2, &protocol.Subscribe{Token: "viewer-tok"})
	if _, ok := recv(t, c2).(*protocol.Snapshot); !ok {
		t.Fatalf("c2 first frame is not snapshot")
	}

	srv.SetRoster([]protocol.RosterEntry{{SceneID: "main", SceneVersion: "sha256:v1"}})

	for i, c := range []*websocket.Conn{c1, c2} {
		roster, ok := recv(t, c).(*protocol.SceneRoster)
		if !ok {
			t.Fatalf("sub %d did not receive roster", i)
		}
		if len(roster.Entries) != 1 || roster.Entries[0].SceneVersion != "sha256:v1" {
			t.Fatalf("sub %d unexpected roster: %+v", i, roster.Entries)
		}
	}
}

// TestRoster_NotSentToLegacy10Subscribers : a 1.0 connection never
// receives the additive scene_roster frame (parity with from_scene_id /
// transition — 1.1-only surface).
func TestRoster_NotSentToLegacy10Subscribers(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	scene := srv.NewScene("main")
	if err := scene.Set(map[string]any{"show.title": "Hello"}); err != nil {
		t.Fatal(err)
	}
	srv.SetRoster([]protocol.RosterEntry{{SceneID: "main", SceneVersion: "sha256:v1"}})

	c := dial(t, url) // 1.0 subprotocol
	send(t, c, &protocol.Subscribe{Token: "viewer-tok"})
	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatalf("first frame is not snapshot")
	}

	// A live update must arrive as the next frame — no roster wedged in.
	if err := scene.Emit(map[string]any{"show.title": "World"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := recv(t, c).(*protocol.Delta); !ok {
		t.Fatalf("1.0 subscriber received a non-delta frame (leaked roster?)")
	}
}
