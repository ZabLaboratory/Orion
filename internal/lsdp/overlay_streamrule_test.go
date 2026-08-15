package lsdp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"

	lproto "github.com/Lumencast/lumencast-go/protocol"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// TestStreamRule_OverlayAppSetReachesRealWire is the ORION-OVERLAY-EFFECTOR-
// STREAM-RULE-PROOF headline proof: a stream-level Blue rule — a program with
// NO scene, NO Show, NO ZabCanvas, nothing but an on-start trigger and a
// core.overlay-app.set@1 node — loads and runs end to end on Engine B
// (bluehost.Host) with the REAL internal/lsdp.Wire wired as its effector, and
// the resulting overlay state is observed on the REAL wire: a genuine WS
// client dialled against wire.Handler() receives the overlay_apps frame.
//
// This is deliberately NOT testParityInventoryOverlay's bar
// (internal/runtime/engine_ab_parity_inventory_test.go), which only proves B
// recorded the same authored intent A did (it reads StepResult.Variables,
// the reserved ctx.variables bag). This test proves the EFFECT: the accumulated
// state a real Solar/Prism consumer would receive over the wire, produced by
// nothing but Blue's existing primitives — no new Zab primitive, no
// stream-rule-specific Blue code.
func TestStreamRule_OverlayAppSetReachesRealWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := quietLogger(t)
	wire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	// NO show, NO Load, NO SetActive anywhere in this test — the stream-rule
	// program below never references a scene id, and bluehost.Host has no
	// concept of one either. The overlay-app control state is stream-level
	// (issue #283 / #292): deliverable with no active scene, exactly as
	// TestLSDP_OverlayAppDeliveredWithoutActiveScene already proves for the
	// legacy Engine A seam.

	h := bluehost.NewHost()
	h.SetOverlayMirror(wire) // *Wire satisfies bluehost.OverlayAppMirror directly

	// Dial BEFORE the program runs, like the sibling Engine-A-seam tests in
	// this file: the join snapshot is consumed first, so the frame this test
	// asserts on can only be the one the stream-rule itself produced.
	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	program := streamRuleFixture(t)
	if err := h.Take("stream-rule-1", "sha256:stream-rule", program, nil, nil, nil); err != nil {
		t.Fatalf("Host.Take: %v", err)
	}
	if _, err := h.Step(bluehost.SlotOnAir); err != nil { // fires the on-start entrypoint
		t.Fatalf("Host.Step (on-start): %v", err)
	}

	frame := readUntilOverlayApps(ctx, t, c)
	app, ok := frame.Apps["stream-rule-overlay"]
	if !ok {
		t.Fatalf("overlay_apps missing stream-rule-overlay: %+v", frame.Apps)
	}
	if app.Running == nil || !*app.Running {
		t.Fatalf("running = %v, want true", app.Running)
	}
	if app.OnAir == nil || !*app.OnAir {
		t.Fatalf("on_air = %v, want true", app.OnAir)
	}
}

// streamRuleFixture reads the hand-composed blue.program.v1 fixture shared
// with internal/bluehost's own dispatch-level tests: on-start ->
// core.overlay-app.set@1, no scene-carrying opcode anywhere in it.
func streamRuleFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../bluespike/testdata/03-overlay-app-stream-rule.program.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}
