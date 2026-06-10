package runtime

import (
	"encoding/json"
	"fmt"
)

// The `animation.play` exec op + the runtime half of the external
// completion contract (ADR 003 §3.1.3 Amendment 1, issue #86).
//
// `animation.play` emits the animation COMMAND as a state write —
// `__anim.<overlay_id>.<generation>` carrying `animation_id`, `params`,
// the per-scene monotone generation counter and the wake key — through
// the scene's own effect seam, so it travels the NORMAL delta pipe and
// is rendered by Solar/CEF (platform doctrine: animations are rendered
// by our engine, never OBS-native). `then` fires IMMEDIATELY on the
// same task; `completed` parks a forked continuation on a
// version+epoch-stamped wake key (the #82/#83 mechanism).
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

// animCommand is the `__anim.<overlay>.<generation>` leaf payload.
// Marshalled from a struct — deterministic field order, never a map.
type animCommand struct {
	AnimationID     string          `json:"animation_id"`
	Params          json.RawMessage `json:"params"`
	Generation      uint64          `json:"generation"`
	DurationSeconds float64         `json:"duration_seconds"`
	WakeKey         string          `json:"wake_key"`
}

// execAnimationPlay implements `animation.play`. Inputs: `overlay_id`,
// `animation_id` (data/config strings), `params` (data/config, raw),
// `duration_seconds` (data/config number — the server-side fallback
// deadline). Out pins: `then` (immediate), `completed` (after report or
// fallback), `error` (report carried a non-null error). On `completed`
// resume, `<node>.result` binds the renderer-supplied payload (absent
// on a fallback resolution).
func execAnimationPlay(s *Scene, t *execTask, node *ExecNode, inPort string) execOpOutcome {
	if inPort == effectCompletePort {
		return finishAnim(s, t, node)
	}

	overlay := pullString(s, t, node, "overlay_id")
	if overlay == "" {
		s.logger.Warn("animation.play without overlay_id", "node", node.ID)
		overlay = "_"
	}
	animID := pullString(s, t, node, "animation_id")
	params, ok := s.pullData(t, node, "params")
	if !ok {
		params = json.RawMessage(`null`)
	}
	secs := s.pullFloat(t, node, "duration_seconds", 0)

	// Per-scene monotone generation counter — scene-goroutine only,
	// never derived from map order: deterministic by construction.
	s.animGeneration++
	gen := s.animGeneration
	key := s.nextWakeKey()

	value, err := json.Marshal(animCommand{
		AnimationID:     animID,
		Params:          params,
		Generation:      gen,
		DurationSeconds: secs,
		WakeKey:         key,
	})
	if err != nil {
		// Unmarshalable params (invalid RawMessage): effect semantics —
		// the chain continues down `error`, nothing is emitted.
		t.env[node.ID+".error"] = mustJSONString("ANIM_COMMAND_ENCODE: " + err.Error())
		if tgt, ok := node.next("error"); ok {
			return execOpOutcome{next: &tgt}
		}
		return execOpOutcome{halt: true}
	}
	// The command travels the NORMAL delta pipe: an internal write of
	// this scene instance's own state (single-writer holds), rendered
	// by Solar/CEF off the delta — not a `__system.*` side channel.
	s.effector.SetLeaf(fmt.Sprintf("__anim.%s.%d", overlay, gen), value)

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
