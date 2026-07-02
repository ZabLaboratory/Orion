package lsdp

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	lproto "github.com/Lumencast/lumencast-go/protocol"

	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// TestLSDP_SceneRosterDelivered proves the Orion Wire maps
// runtime.RosterEntry onto the kit's scene_roster frame end to end: a
// fresh 1.1 subscriber receives the cached roster right after its
// snapshot, and a live EmitRoster fans an update out to it.
func TestLSDP_SceneRosterDelivered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, _, wsURL := dualShow(t) // Load + SetActive already cached a roster.

	c := dialLSDP(ctx, t, wsURL, "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")

	// 1. Snapshot, then the cached roster replayed after it.
	if _, ok := readServerFrameRaw(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatalf("first frame: want snapshot")
	}
	roster, ok := readServerFrameRaw(ctx, t, c).(*lproto.SceneRoster)
	if !ok {
		t.Fatalf("second frame: want scene_roster")
	}
	if len(roster.Entries) != 1 ||
		roster.Entries[0].SceneID != "scene-1" ||
		roster.Entries[0].SceneVersion != "sha256:test-1" {
		t.Fatalf("unexpected initial roster: %+v", roster.Entries)
	}

	// 2. A live roster update fans out to the connected subscriber.
	wire.EmitRoster([]runtime.RosterEntry{
		{SceneID: "scene-1", SceneVersion: "sha256:test-1"},
		{SceneID: "scene-2", SceneVersion: "sha256:test-2"},
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		frame := readServerFrameRaw(ctx, t, c)
		upd, ok := frame.(*lproto.SceneRoster)
		if !ok {
			if time.Now().After(deadline) {
				t.Fatalf("no updated roster before deadline (last %T)", frame)
			}
			continue
		}
		if len(upd.Entries) != 2 || upd.Entries[1].SceneID != "scene-2" {
			t.Fatalf("unexpected updated roster: %+v", upd.Entries)
		}
		return
	}
}
