package runtime

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Tests for `animation.play` (ADR 003 §3.1.3 Amendment 1, issue #86;
// leaf shape amended by ADR 011 §3.2/I3): the SCALAR generation leaf
// `__anim.<overlay>` = uint64 generation counter in the normal delta
// pipe, `then` immediate, `completed` parked on a stamped wake key,
// the server-side duration fallback on the timer wheel, first-of-two
// resolution (the loser is an inert counted drop), and the -0 pattern
// on the duration.
//
// I3 NOTE. The leaf is no longer an object carrying `animation_id` /
// `params` / `wake_key`: those are compile-resolved (asset geometry) and
// kept server-side (wake key in the parked map). The wake key is the
// deterministic `wk|<sceneVersion>|<epoch>|<seq>` (exec_timer.go), with
// seq == generation here (one play == one nextWakeKey == one generation),
// so a test reconstructs it from the scalar leaf rather than reading it
// off the wire — exactly as the SERVER resolves it, since neither the
// park nor the fallback nor resumeParked reads the leaf.

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
			"variable": raw(`"` + name + `"`), "value": raw(valueJSON),
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

// animScalarGen reads the SCALAR generation counter off the leaf
// `__anim.<overlay>` (ADR 011 §3.2): a bare uint64, never an object.
func animScalarGen(t *testing.T, sc *Scene, leaf string) uint64 {
	t.Helper()
	raw, ok := sc.state.Get(leaf)
	if !ok {
		t.Fatalf("leaf %s absent", leaf)
	}
	var gen uint64
	if err := json.Unmarshal(raw, &gen); err != nil {
		t.Fatalf("leaf %s is not a scalar uint64 (ADR 011 §3.2): %s (%v)", leaf, raw, err)
	}
	return gen
}

// animWakeKeyForGen rebuilds the deterministic wake key for a generation
// the SAME way the server mints it (exec_timer.go nextWakeKey:
// wk|<sceneVersion>|<epoch>|<seq>) — at start epoch is 0 and seq ==
// generation (one play arms one wake key). With the scalar leaf the wake
// key no longer travels the wire, so a reporting client / test recovers
// it server-side; this mirrors that recovery for the parked-map resume.
func animWakeKeyForGen(sc *Scene, gen uint64) string {
	return fmt.Sprintf("wk|%s|%d|%d", sc.graph.SceneVersion, sc.execEpoch, gen)
}

// animLeafKey recovers the wake key for the LATEST play on a leaf: read
// the scalar generation, rebuild the deterministic key. Convenience for
// the single-play tests (the leaf still holds the only generation).
func animLeafKey(t *testing.T, sc *Scene, leaf string) string {
	t.Helper()
	return animWakeKeyForGen(sc, animScalarGen(t, sc, leaf))
}

// TestAnim_EmitsScalarGenerationAndThenImmediate: the trigger is a
// SCALAR state write — `__anim.ov` = generation 1, a bare uint64, NOT an
// object (ADR 011 §3.2: no animation_id/params/wake_key on the wire) —
// `then` fires immediately, `completed` stays parked.
func TestAnim_EmitsScalarGenerationAndThenImmediate(t *testing.T) {
	sc, _, m := animScene(t, "anim-emit", "100")
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.after", `"then-fired"`, 2*time.Second)

	// The leaf is the scalar generation `1` — byte-exact, no object.
	waitForState(t, sc, "__anim.ov", `1`, 2*time.Second)
	if gen := animScalarGen(t, sc, "__anim.ov"); gen != 1 {
		t.Fatalf("scalar generation = %d, want 1", gen)
	}

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

	key := animLeafKey(t, sc, "__anim.ov")
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

	key := animLeafKey(t, sc, "__anim.ov")
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

	key := animLeafKey(t, sc, "__anim.ov")
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
	key := animLeafKey(t, sc, "__anim.ov")

	sc.CancelExec()
	waitFor(t, "cancellation", func() bool { _, _, p := m.counts(); return p == 0 })

	sc.Input(InputMsg{ResumeExec: key, ResumeEnv: AnimReportEnv(raw(`1`), ""), Source: "test:late"})
	waitFor(t, "stale drop", func() bool { return m.stale() == 1 })
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		t.Fatalf("stale report resumed a continuation: %s", v)
	}
}

// TestAnim_GenerationMonotonic: two plays bump the SAME scalar leaf
// `__anim.ov` 1 → 2 (per-scene monotone counter, never map order) — each
// delta is a distinct KeyframePlayer replay trigger (ADR 011 §3.2 /
// criterion #5 "replay on re-fire") — and mint distinct stamped wake
// keys. Both plays write one leaf (the scalar overwrites), so the final
// leaf value is 2; the two wake keys are the deterministic per-generation
// keys the server parked.
func TestAnim_GenerationMonotonic(t *testing.T) {
	sc, _, m := animScene(t, "anim-gen", "100")
	startScene(t, sc)
	mustFire(t, sc, "e")
	mustFire(t, sc, "e")
	waitFor(t, "two parks", func() bool { _, _, p := m.counts(); return p == 2 })

	// The scalar leaf advanced 1 → 2 (the second delta re-triggers replay).
	if gen := animScalarGen(t, sc, "__anim.ov"); gen != 2 {
		t.Fatalf("scalar generation = %d, want 2 (two plays bump the same leaf)", gen)
	}
	k1 := animWakeKeyForGen(sc, 1)
	k2 := animWakeKeyForGen(sc, 2)
	if k1 == k2 {
		t.Fatalf("wake keys not distinct: %s", k1)
	}
	if k1 != "wk|sha256:effects-test|0|1" || k2 != "wk|sha256:effects-test|0|2" {
		t.Fatalf("non-deterministic wake keys: %s / %s", k1, k2)
	}
}
