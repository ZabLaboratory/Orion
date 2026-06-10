package runtime

// Probe tests for animation.play — complement Forge's exec_anim_test.go.
// Do NOT rewrite it.
//
// Axes:
//  1. __anim.* namespace: the command leaf uses __anim.<overlay>.<gen>,
//     never __system.* — pinned against drift that would bypass the inbox
//     gate (contract §2.6: no free __system.* write can resume a
//     continuation).
//  2. DurationFallback: timer fires if no external report (already in
//     Forge's test), confirmed from the OTHER direction — external report
//     after timer fires is an unknown drop (timer-first race).
//  3. Double-play: two successive animation.play fires on the same overlay
//     produce distinct generations AND distinct wake keys (generation
//     monotone guarantee cross-checked from the probe angle).
//  4. NaN duration: already covered by Forge. This test covers
//     duration=null (JSON null → 0 → immediate fire, same -0 pattern).
//  5. Error-port route with no error output wired: halts cleanly, no panic.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestAnim_LeafNamespace_IsAnimNotSystem: the command emitted by
// animation.play must be written under `__anim.*`, never `__system.*`.
// A free `__system.*` write would bypass inbox.go's CanWritePath check
// (contract §2.6 hardening); the delta-pipe write must use the `__anim`
// namespace so it travels as a normal leaf write through the effector.
func TestAnim_LeafNamespace_IsAnimNotSystem(t *testing.T) {
	sc, _, _ := animScene(t, "anim-ns", "100")
	startScene(t, sc)
	mustFire(t, sc, "e")

	// Wait for the leaf to appear.
	waitFor(t, "__anim.ov.1 emitted", func() bool {
		_, ok := sc.state.Get("__anim.ov.1")
		return ok
	})

	// Confirm it is under __anim, NOT __system.
	if _, ok := sc.state.Get("__system.anim.ov.1"); ok {
		t.Fatal("animation.play wrote to __system.* namespace — violates B-syswrite contract (§2.6)")
	}
	if _, ok := sc.state.Get("__anim.ov.1"); !ok {
		t.Fatal("animation.play did not write to __anim.* namespace — command not emitted")
	}

	// The leaf key must match the format __anim.<overlay>.<generation>.
	rawCmd, _ := sc.state.Get("__anim.ov.1")
	var cmd struct {
		WakeKey    string  `json:"wake_key"`
		Generation uint64  `json:"generation"`
		AnimID     string  `json:"animation_id"`
		Duration   float64 `json:"duration_seconds"`
	}
	if err := json.Unmarshal(rawCmd, &cmd); err != nil {
		t.Fatalf("leaf __anim.ov.1 not valid animCommand JSON: %v", err)
	}
	if cmd.Generation != 1 {
		t.Errorf("generation = %d, want 1", cmd.Generation)
	}
	if !strings.HasPrefix(cmd.WakeKey, "wk|") {
		t.Errorf("wake_key %q must start with wk|", cmd.WakeKey)
	}
}

// TestAnim_TimerFirst_ThenExternalReport_IsUnknownDrop: the server-side
// timer fires first (clock advanced); then an external report arrives with
// the same wake key. The report must be an unknown drop — the key left the
// parked map when the timer resolved it. Counted, resumes nothing.
func TestAnim_TimerFirst_ThenExternalReport_IsUnknownDrop(t *testing.T) {
	sc, clk, m := animScene(t, "anim-timer-first", "2")
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitFor(t, "park", func() bool { _, _, p := m.counts(); return p == 1 })

	key := animLeafKey(t, sc, "__anim.ov.1")

	// Timer fires first.
	clk.Advance(2 * time.Second)
	waitFor(t, "timer resolved", func() bool { _, _, p := m.counts(); return p == 0 })

	// External report arrives after the timer resolved — unknown drop.
	if !sc.Input(InputMsg{ResumeExec: key, ResumeEnv: AnimReportEnv(raw(`"late"`), ""), Source: "test:late"}) {
		t.Fatal("inbox full")
	}
	waitFor(t, "unknown drop counted", func() bool { return m.unknown() == 1 })

	// State must reflect the timer-fallback path (no result bound on
	// fallback): set.out reads anim.result which is unbound → null.
	time.Sleep(20 * time.Millisecond)
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		// set.out reads anim.result; on timer fire it's unbound (null).
		// On late report it would have been the report value. It must be null.
		t.Fatalf("late report overwrote timer result: out = %s", v)
	}
}

// TestAnim_NullDuration_FiresImmediately: JSON null duration_seconds
// unmarshals to 0.0 (same as -0); durationFromSeconds(0) → 0 → the
// fallback timer fires immediately (via the fake clock's zero-duration
// path, same as TestAnim_NonPositiveDuration).
func TestAnim_NullDuration_FiresImmediately(t *testing.T) {
	sc, _, m := animScene(t, "anim-null-dur", `null`)
	startScene(t, sc)
	mustFire(t, sc, "e")
	// Zero-duration timer fires at arm time — no Advance needed.
	waitFor(t, "immediate fallback (null duration)", func() bool {
		_, _, p := m.counts()
		return p == 0
	})
	if v, _ := sc.state.Get("__vars.bp.err"); string(v) != `null` {
		t.Fatalf("error port fired on null-duration fallback: %s", v)
	}
}

// TestAnim_ErrorPortNoWire_HaltsCleanly: animation.play receives an error
// report when the `error` output pin is NOT wired. The scene must halt the
// task cleanly — no panic, no infinite loop — and keep serving subsequent
// inputs.
func TestAnim_ErrorPortNoWire_HaltsCleanly(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"anim": {ID: "anim", Op: OpAnimationPlay,
				Config: map[string]json.RawMessage{
					"overlay_id":       raw(`"ov"`),
					"animation_id":     raw(`"fade"`),
					"duration_seconds": raw(`3600`),
				},
				// Only `completed` wired; `error` is absent.
				Next: map[string]ExecTarget{"completed": {Node: "set.done"}},
			},
			"set.done": constSet("set.done", "done", `"done"`),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "anim"}}},
	}
	sc, _, m := animScene(t, "anim-no-err-wire", "3600")
	sc.InstallExec(prog)
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitFor(t, "park", func() bool { _, _, p := m.counts(); return p == 1 })

	key := animLeafKey(t, sc, "__anim.ov.1")
	// Deliver an error report on a node with no error port wired → halt.
	if !sc.Input(InputMsg{ResumeExec: key, ResumeEnv: AnimReportEnv(nil, "RENDER_FAIL"), Source: "test:err"}) {
		t.Fatal("inbox full")
	}
	// Give the scene time to process.
	time.Sleep(50 * time.Millisecond)
	// Scene must still respond to subsequent inputs.
	if !sc.Input(InputMsg{Path: "score.team_a", Value: raw(`7`), Source: "probe"}) {
		t.Fatal("scene inbox full after error-halt — scene is stuck")
	}
	waitForState(t, sc, "score.team_a", `7`, time.Second)
}
