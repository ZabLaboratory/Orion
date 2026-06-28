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

// readUntilSlot reads server frames until one carries the slot leaf, or fails.
// Accepts the binding from either a Snapshot's State or a Delta's Patches.
func readUntilSlot(ctx context.Context, t *testing.T, c *websocket.Conn, leaf string) string {
	t.Helper()
	for i := 0; i < 8; i++ {
		switch m := readServerFrame(ctx, t, c).(type) {
		case *lproto.Snapshot:
			if v, ok := m.State[leaf]; ok {
				return string(v)
			}
		case *lproto.Delta:
			for _, p := range m.Patches {
				if p.Path == leaf {
					return string(p.Value)
				}
			}
		}
	}
	t.Fatalf("slot leaf %q never reached the wire", leaf)
	return ""
}

// TestLSDP_SlotAssignmentEmitsDelta (RC4): a stream-level slot assignment
// emits an LSDP delta keyed `__cam.slots.<slot_ref>` = "<peer_label>" on the
// active wire — the binding Solar #28 re-keys the `meet.peer` from, without a
// scene switch or re-push.
func TestLSDP_SlotAssignmentEmitsDelta(t *testing.T) {
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
	// Drain the join snapshot.
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	// A slot is assigned (post-2xx ZabCam upsert, fired by the runtime seam).
	wire.EmitSlotAssignment("cam-left", "alice")

	if got := readUntilSlot(ctx, t, c, "__cam.slots.cam-left"); got != `"alice"` {
		t.Fatalf("slot delta = %s, want \"alice\"", got)
	}
}

// TestLSDP_SlotAssignmentPersistsAcrossSceneSwitch (RC5): a slot binding is
// stream-level — it survives a switch of active scene. A viewer joining the
// NEW active scene after the switch gets the slot in its keyframe, even though
// no scene binds the `__cam.slots.*` leaf (it rides the wire above the
// per-scene bound-leaf gate).
func TestLSDP_SlotAssignmentPersistsAcrossSceneSwitch(t *testing.T) {
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

	// Assigned while A is active.
	wire.EmitSlotAssignment("cam-left", "alice")

	// Operator switches to B (a different scene of the same stream).
	show.Load("scene-b", passthroughGraph("scene-b", "sha256:ver-b", "board.row0"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-b"})
	if err := show.SetActive("scene-b", nil); err != nil {
		t.Fatalf("SetActive B: %v", err)
	}

	// A viewer joins AFTER the switch: B's keyframe must still carry the slot.
	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatal("first frame must be the join snapshot")
	}
	if snap.SceneID != "scene-b" {
		t.Fatalf("keyframe scene_id = %q, want scene-b (active)", snap.SceneID)
	}
	if got := string(snap.State["__cam.slots.cam-left"]); got != `"alice"` {
		t.Fatalf("slot lost across scene switch: keyframe leaf = %q, want \"alice\"", got)
	}
}

// TestLSDP_SlotAssignmentIsolatedPerStream: a slot binding on one stream's
// wire never leaks onto another stream's wire — each Wire (one per stream)
// holds its own derived cache.
func TestLSDP_SlotAssignmentIsolatedPerStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := quietLogger(t)
	wireA, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire A: %v", err)
	}
	wireB, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire B: %v", err)
	}
	for _, w := range []*Wire{wireA, wireB} {
		show := runtime.NewShow(runtime.NewComputeRegistry(), logger)
		show.SetMirrors(w)
		t.Cleanup(show.Stop)
		show.Load("s", passthroughGraph("s", "sha256:v", "score.team_a"),
			&compiler.RenderBundle{SceneVersion: "sha256:v"})
		if err := show.SetActive("s", nil); err != nil {
			t.Fatalf("SetActive: %v", err)
		}
	}

	// Assign only on stream A.
	wireA.EmitSlotAssignment("cam-left", "alice")

	// Stream B's late joiner must NOT see A's binding.
	c := dialLSDP(ctx, t, mountWire(t, wireB), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatal("first frame must be the join snapshot")
	}
	if _, leaked := snap.State["__cam.slots.cam-left"]; leaked {
		t.Fatalf("stream B leaked stream A's slot binding: %v", snap.State)
	}
	// Sanity: it IS present on A.
	time.Sleep(20 * time.Millisecond)
	cA := dialLSDP(ctx, t, mountWire(t, wireA), "viewer", 0)
	defer cA.Close(websocket.StatusNormalClosure, "")
	if got := readUntilSlot(ctx, t, cA, "__cam.slots.cam-left"); got != `"alice"` {
		t.Fatalf("stream A slot = %s, want \"alice\"", got)
	}
}
