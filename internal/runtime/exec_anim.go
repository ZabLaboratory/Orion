package runtime

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// The `animation.play` exec op + the runtime half of the external
// completion contract (ADR 003 §3.1.3 Amendment 1, issue #86; leaf shape
// amended by ADR 011 §3.2/I3).
//
// `animation.play` emits the animation TRIGGER as a state write — the
// SCALAR generation leaf `__anim.<overlay_id>` = the per-scene monotone
// generation counter (a bare uint64) — through the scene's own effect
// seam, so it travels the NORMAL delta pipe and passes the LSDP §3.2.1
// scalar-only filter. The lowered keyframe RenderNode (compiler
// lower_animation.go, keyed on `__anim.<overlay_id>`) is what Solar's
// KeyframePlayer remounts on every delta of this scalar → it replays the
// authored, compile-resolved geometry (ADR 011 §3.3). The `animation_id`
// and `params` are NO LONGER on the leaf — they are authored geometry +
// compile-time-resolved overrides, lowered into the keyframe node by the
// compiler (ADR 011 §3.2, the A5.5 "leaf carries no node shape"
// invariant). `then` fires IMMEDIATELY on the same task; `completed`
// parks a forked continuation on a version+epoch-stamped wake key (the
// #82/#83 mechanism).
//
// ADR 011 §3.6 / I3 — completion machinery UNCHANGED. The park
// continuation (out.parkKey = key) + the duration fallback on the timer
// wheel + the external-report resume (resumeParked) all live SERVER-SIDE
// and key off the parked map — NONE of them read the `__anim` leaf. So
// scalarising the leaf does not touch the #82/#83/#86 mechanism: the
// duration fallback (the only resolver any prod renderer exercises today)
// is leaf-independent. The `duration_seconds` input still arms the
// server-side fallback timer; it never needed to ride the leaf.
//
// ⚠ Forward contract (I5, Conduit). The phase-3 wire contract §2.1 has
// the external-report renderer learn its `wake_key` by reading it off the
// OBJECT leaf `__anim.<overlay>.<gen>`. That object channel is gone after
// I3, so the external-report path (#86, R9-dormant — Solar carries NO
// completion-reporting code today) needs a replacement wake-key channel.
// That is the LSDP/render-bundle wire shape I5 owns; this op does not
// invent one (would be speculative architecture). See the PR note.
//
// Two resolvers race, first one wins (§2.5 of the phase-3 wire
// contract):
//   - the renderer's external report, delivered through the dedicated
//     authenticated endpoint (internal/api/exec_completion.go) as
//     scene.Input(InputMsg{ResumeExec}) — NEVER a free `__system.*`
//     write (B-syswrite);
//   - the server-side duration fallback on the timer wheel
//     (exec_timer.go), armed at play time — covers broadcast viewers
//     that cannot report.
// The loser arrives after the wake key left the parked map and is
// dropped as an unknown resume (logged + counted, resumes nothing) —
// idempotent double-resolution by construction.
//
// R9 dormancy: like every exec op, this runs only on scenes with an
// installed ExecProgram — no production path installs one until the
// phase-4 gate (#87). The state write itself goes through the scene's
// own effector (intra-goroutine), never the cross-scene inbox fan-out.

// OpAnimationPlay is the runtime-canonical op name.
const OpAnimationPlay = "animation.play"

// animReportEnvKey is the FIXED task-env slot an external animation
// report lands in. Unlike the #85 intra-process effects (whose worker
// knows the node id and uses effectEnvKey), the external reporter only
// echoes an opaque wake key — but a parked continuation is a dedicated
// forked task, so a fixed slot can never collide.
const animReportEnvKey = "__anim.report"

// AnimReportEnv shapes an external completion report (result + error)
// into the ResumeEnv the completion endpoint delivers with
// InputMsg{ResumeExec}. errStr != "" routes the resumed chain down the
// node's `error` port (effect semantics, never a crash).
func AnimReportEnv(result json.RawMessage, errStr string) map[string]json.RawMessage {
	raw, err := json.Marshal(effectEnvelope{Value: result, Err: errStr})
	if err != nil {
		raw = json.RawMessage(`{"error":"ANIM_REPORT_MARSHAL"}`)
	}
	return map[string]json.RawMessage{animReportEnvKey: raw}
}

// execAnimationPlay implements `animation.play`. Inputs: `overlay_id`,
// `duration_seconds` (data/config number — the server-side fallback
// deadline). `animation_id` / `params` are NO LONGER read here: the asset
// geometry is resolved at compile time into the lowered keyframe node
// (ADR 011 §3.2/§3.3), not carried on the wire. Out pins: `then`
// (immediate), `completed` (after report or fallback), `error` (report
// carried a non-null error). On `completed` resume, `<node>.result` binds
// the renderer-supplied payload (absent on a fallback resolution).
func execAnimationPlay(s *Scene, t *execTask, node *ExecNode, inPort string) execOpOutcome {
	if inPort == effectCompletePort {
		return finishAnim(s, t, node)
	}

	overlay := pullString(s, t, node, "overlay_id")
	if overlay == "" {
		s.logger.Warn("animation.play without overlay_id", "node", node.ID)
		overlay = "_"
	}
	secs := s.pullFloat(t, node, "duration_seconds", 0)

	// Per-scene monotone generation counter — scene-goroutine only,
	// never derived from map order: deterministic by construction. The
	// wake key (kept SERVER-SIDE for the park/fallback) is minted before
	// the leaf write so its sequence ordering is unchanged vs pre-I3.
	s.animGeneration++
	gen := s.animGeneration
	key := s.nextWakeKey()

	// ADR 011 §3.2: the leaf is the SCALAR generation counter (a bare
	// uint64), NOT an object. It travels the NORMAL delta pipe — an
	// internal write of this scene instance's own state (single-writer
	// holds) — passes the LSDP §3.2.1 scalar-only filter, and its
	// value-change is the M9 replay trigger Solar's KeyframePlayer keys
	// on (the lowered `__anim.<overlay>` keyframe node). A uint64's JSON
	// form is just its decimal digits, so the value is built directly —
	// there is no encode-error branch (the old object-marshal error path
	// is gone with the object payload).
	value := json.RawMessage(strconv.FormatUint(gen, 10))
	s.effector.SetLeaf(fmt.Sprintf("__anim.%s", overlay), value)

	out := execOpOutcome{}
	if tgt, ok := node.next("then"); ok {
		// `then` fires immediately on the SAME task (Amendment 1).
		out.next = &tgt
	}
	if _, ok := node.next("completed", "error"); ok {
		// Park the completion continuation + arm the duration fallback.
		// durationFromSeconds maps negative / IEEE-754 -0 / NaN to 0:
		// the fallback then fires immediately — defined, tested, never
		// an infinite wait (the -0 pattern, like delay/#83).
		out.park = true
		out.parkKey = key
		out.resume = ExecTarget{Node: node.ID, Port: effectCompletePort}
		out.timer = true
		out.deadline = s.clock.Now().Add(durationFromSeconds(secs))
	}
	return out
}

// finishAnim handles the completion re-entry. A duration-fallback
// resume carries no report slot — plain `completed`, no result bound.
func finishAnim(s *Scene, t *execTask, node *ExecNode) execOpOutcome {
	var env effectEnvelope
	if raw, ok := t.env[animReportEnvKey]; ok {
		_ = json.Unmarshal(raw, &env)
		delete(t.env, animReportEnvKey)
	}
	if env.Err != "" {
		t.env[node.ID+".error"] = mustJSONString(env.Err)
		s.logger.Warn("animation.play reported error", "node", node.ID, "err", env.Err)
		if tgt, ok := node.next("error"); ok {
			return execOpOutcome{next: &tgt}
		}
		return execOpOutcome{halt: true}
	}
	if env.Value != nil {
		t.env[node.ID+".result"] = env.Value
	}
	if tgt, ok := node.next("completed"); ok {
		return execOpOutcome{next: &tgt}
	}
	return execOpOutcome{halt: true}
}
