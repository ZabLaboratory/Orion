package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Tests for `animation.play` (ADR 003 §3.1.3 Amendment 1, issue #86):
// the `__anim.<overlay>.<generation>` state write in the normal delta
// pipe, `then` immediate, `completed` parked on a stamped wake key,
// the server-side duration fallback on the timer wheel, first-of-two
// resolution (the loser is an inert counted drop), and the -0 pattern
// on the duration.

// animNode builds an animation.play node with config inputs.
func animNode(id string, durationJSON string, next map[string]ExecTarget) *ExecNode {
	return &ExecNode{
		ID: id, Op: OpAnimationPlay,
		Config: map[string]json.RawMessage{
			"overlay_id":       raw(`"ov"`),
			"animation_id":     raw(`"fade"`),
			"params":           raw(`{"x":1}`),
			"duration_seconds": raw(durationJSON),
		},
		Next: next,
	}
}

// constSet builds a variable.set writing a constant (config value).
func constSet(id, name, valueJSON string) *ExecNode {
	return &ExecNode{
		ID: id, Op: OpVariableSet,
		Config: map[string]json.RawMessage{
			"name": raw(`"` + name + `"`), "value": raw(valueJSON),
		},
	}
}

func animProg(durationJSON string) *ExecProgram {
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"anim": animNode("anim", durationJSON, map[string]ExecTarget{
				"then":      {Node: "set.after"},
				"completed": {Node: "set.out"},
				"error":     {Node: "set.err"},
			}),
			"set.after": constSet("set.after", "after", `"then-fired"`),
			"set.out":   setFromPin("set.out", "out", "anim", "result", nil),
			"set.err":   setFromPin("set.err", "err", "anim", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "anim"}}},
	}
}

func animScene(t *testing.T, id, durationJSON string) (*Scene, *fakeClock, *fakeExecMetrics) {
	t.Helper()
	sc := NewScene(id, effectsGraph(id), &compiler.RenderBundle{SceneVersion: "sha256:effects-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(animProg(durationJSON))
	clk := newFakeClock()
	sc.SetClock(clk)
	m := &fakeExecMetrics{}
	sc.SetExecMetrics(m)
	return sc, clk, m
}

// animLeafKey reads the wake key back out of the emitted command —
// exactly what the renderer does (it never mints one).
func animLeafKey(t *testing.T, sc *Scene, leaf string) string {
	t.Helper()
	rawCmd, ok := sc.state.Get(leaf)
	if !ok {
		t.Fatalf("leaf %s absent", leaf)
	}
	var cmd struct {
		WakeKey string `json:"wake_key"`
	}
	if err := json.Unmarshal(rawCmd, &cmd); err != nil || cmd.WakeKey == "" {
		t.Fatalf("leaf %s carries no wake key: %s", leaf, rawCmd)
	}
	return cmd.WakeKey
}

// TestAnim_EmitsCommandAndThenImmediate: the command is a state write
// with deterministic shape (generation 1, stamped wake key), `then`
// fires immediately, `completed` stays parked.
func TestAnim_EmitsCommandAndThenImmediate(t *testing.T) {
	sc, _, m := animScene(t, "anim-emit", "100")
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.after", `"then-fired"`, 2*time.Second)

	want := `{"animation_id":"fade","params":{"x":1},"generation":1,` +
		`"duration_seconds":100,"wake_key":"wk|sha256:effects-test|0|1"}`
	waitForState(t, sc, "__anim.ov.1", want, 2*time.Second)

	waitFor(t, "completed continuation parked", func() bool {
		_, _, parked := m.counts()
		return parked == 1
	})
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		t.Fatalf("completed fired without report: %s", v)
	}
}

// TestAnim_ExternalReportResumesCompleted: the renderer-style report
// (ResumeExec + AnimReportEnv via the inbox) resumes the parked
// continuation and binds `<node>.result`.
func TestAnim_ExternalReportResumesCompleted(t *testing.T) {
	sc, _, m := animScene(t, "anim-report", "100")
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitFor(t, "park", func() bool { _, _, p := m.counts(); return p == 1 })

	key := animLeafKey(t, sc, "__anim.ov.1")
	if !sc.Input(InputMsg{ResumeExec: key, ResumeEnv: AnimReportEnv(raw(`{"ok":true}`), ""), Source: "test:report"}) {
		t.Fatal("inbox full")
	}
	waitForState(t, sc, "__vars.bp.out", `{"ok":true}`, 2*time.Second)
	if v, _ := sc.state.Get("__vars.bp.err"); string(v) != `null` {
		t.Fatalf("error port fired on success: %s", v)
	}
}

// TestAnim_ReportErrorRoutesErrorPort: a non-null report error resumes
// down `error` (effect semantics, never a crash).
func TestAnim_ReportErrorRoutesErrorPort(t *testing.T) {
	sc, _, m := animScene(t, "anim-err", "100")
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitFor(t, "park", func() bool { _, _, p := m.counts(); return p == 1 })

	key := animLeafKey(t, sc, "__anim.ov.1")
	sc.Input(InputMsg{ResumeExec: key, ResumeEnv: AnimReportEnv(nil, "RENDER_FAIL"), Source: "test:report"})
	waitForState(t, sc, "__vars.bp.err", `"RENDER_FAIL"`, 2*time.Second)
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		t.Fatalf("completed fired on error: %s", v)
	}
}

// TestAnim_DurationFallbackFires: with no reporting client, the timer
// wheel resolves `completed` at the authored duration (no result bound).
func TestAnim_DurationFallbackFires(t *testing.T) {
	sc, clk, m := animScene(t, "anim-fallback", "5")
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitFor(t, "park", func() bool { _, _, p := m.counts(); return p == 1 })

	clk.Advance(5 * time.Second)
	// completed resumes with NO report slot: set.out pulls the unbound
	// `anim.result` pin and writes null — observe via the parked gauge
	// and the unchanged error leaf instead.
	waitFor(t, "fallback resume", func() bool { _, _, p := m.counts(); return p == 0 })
	if v, _ := sc.state.Get("__vars.bp.err"); string(v) != `null` {
		t.Fatalf("error port fired on fallback: %s", v)
	}
	if m.unknown() != 0 || m.stale() != 0 {
		t.Fatalf("fallback resume counted as drop: unknown=%d stale=%d", m.unknown(), m.stale())
	}
}

// TestAnim_ReportBeforeTimeout_TimerBecomesInertDrop: the external
// report wins; the later timer fire is an unknown-wake-key drop —
// counted, resumes nothing (idempotent double resolution).
func TestAnim_ReportBeforeTimeout_TimerBecomesInertDrop(t *testing.T) {
	sc, clk, m := animScene(t, "anim-race", "5")
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitFor(t, "park", func() bool { _, _, p := m.counts(); return p == 1 })

	key := animLeafKey(t, sc, "__anim.ov.1")
	sc.Input(InputMsg{ResumeExec: key, ResumeEnv: AnimReportEnv(raw(`"r"`), ""), Source: "test:report"})
	waitForState(t, sc, "__vars.bp.out", `"r"`, 2*time.Second)

	clk.Advance(5 * time.Second)
	waitFor(t, "stale timer drop counted", func() bool { return m.unknown() == 1 })
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `"r"` {
		t.Fatalf("timer overwrote the report result: %s", v)
	}
}

// TestAnim_NonPositiveDuration: negative and IEEE-754 -0 durations are
// DEFINED — durationFromSeconds maps them to 0 and the fallback fires
// immediately. No panic, no infinite wait (the -0 pattern, like #83).
func TestAnim_NonPositiveDuration(t *testing.T) {
	for name, dur := range map[string]string{"minus-zero": `-0`, "negative": `-3`} {
		t.Run(name, func(t *testing.T) {
			sc, _, m := animScene(t, "anim-"+name, dur)
			startScene(t, sc)
			mustFire(t, sc, "e")
			// The fake clock fires a 0-duration timer at arm time: the
			// fallback resolves without any Advance.
			waitForState(t, sc, "__vars.bp.after", `"then-fired"`, 2*time.Second)
			waitFor(t, "immediate fallback", func() bool { _, _, p := m.counts(); return p == 0 })
			if v, _ := sc.state.Get("__vars.bp.err"); string(v) != `null` {
				t.Fatalf("error port fired: %s", v)
			}
		})
	}
}

// TestAnim_StaleEpochReportDropped: cancellation bumps the epoch; a
// report carrying the old stamped key is dropped by the inner
// version+epoch gate (gate 3) — counted, resumes nothing.
func TestAnim_StaleEpochReportDropped(t *testing.T) {
	sc, _, m := animScene(t, "anim-stale", "100")
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitFor(t, "park", func() bool { _, _, p := m.counts(); return p == 1 })
	key := animLeafKey(t, sc, "__anim.ov.1")

	sc.CancelExec()
	waitFor(t, "cancellation", func() bool { _, _, p := m.counts(); return p == 0 })

	sc.Input(InputMsg{ResumeExec: key, ResumeEnv: AnimReportEnv(raw(`1`), ""), Source: "test:late"})
	waitFor(t, "stale drop", func() bool { return m.stale() == 1 })
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		t.Fatalf("stale report resumed a continuation: %s", v)
	}
}

// TestAnim_GenerationMonotonic: two plays mint generations 1 then 2
// (per-scene monotone counter, never map order) with distinct stamped
// wake keys.
func TestAnim_GenerationMonotonic(t *testing.T) {
	sc, _, m := animScene(t, "anim-gen", "100")
	startScene(t, sc)
	mustFire(t, sc, "e")
	mustFire(t, sc, "e")
	waitFor(t, "two parks", func() bool { _, _, p := m.counts(); return p == 2 })

	k1 := animLeafKey(t, sc, "__anim.ov.1")
	k2 := animLeafKey(t, sc, "__anim.ov.2")
	if k1 == k2 {
		t.Fatalf("wake keys not distinct: %s", k1)
	}
	if k1 != "wk|sha256:effects-test|0|1" || k2 != "wk|sha256:effects-test|0|2" {
		t.Fatalf("non-deterministic wake keys: %s / %s", k1, k2)
	}
}
