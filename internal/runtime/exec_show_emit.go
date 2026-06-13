package runtime

import (
	"encoding/json"
)

// The `show.emit` exec op — the rule→antenna bridge primitive (ADR 009
// §3.6, the 82nd Blue stdlib node, seed `core.show.emit@1`, Blue#73).
//
// Semantics (NORMATIVE, ADR 009 §3.6). The op reads config `topic` and
// data input `payload`, then injects a SYSTEM write `__events.<topic>` =
// payload into the ACTIVE scene ONLY (`show.Active()`), via the Show's
// audited inbox path (the `s.emitEvent` seam wired at Load). This is a NEW
// runtime path DISTINCT from the §3.3 routing union: it deliberately does
// NOT pass through `RouteTargets()`. If it followed the union, a promoted
// stream rule's emission would fan out to every OTHER promoted rule and
// cascade rule→rule (loops). Routing rule→active-only is the anti-loop
// invariant by construction:
//
//   - emit rule→active: targets show.Active() alone — never rule→rule, no
//     cascade possible;
//   - a `__events.*` write off the wire (operator / service, e.g. Cosmos)
//     still follows the §3.3 union — external origin, no loop.
//
// From the active scene the node is reflexive: it can refire the active
// scene's own on-event entries (a useful no-op-loop, documented in the
// seed). The on-event firing happens inside the active scene's own
// applyInput (scene.go) when the injected `__events.<topic>` write lands —
// the same path an external event takes.
//
// Construction-safe, NO error pin (Blue#73 contract): delivery to the
// active scene's system inbox is build-safe (unlike http.request/db.query
// which carry transport errors), so the seed declares a single `then` exec
// output and no `error`. `then` fires synchronously on the same task. If a
// real delivery-failure semantics ever emerges, that is a NEW signature
// revision routed through Atlas/Conduit — never a silent reinterpretation
// of this contract (per the brief).
//
// R9 dormancy: like every exec op, it runs only on a scene with an
// installed ExecProgram, which no production path installs before the
// phase-4 gate (#87). The injection seam (s.emitEvent) is nil-safe: an
// unwired scene (no active reachable, or pre-wiring) no-ops the injection
// and STILL fires `then` — construction-safe, never a halt.

// OpShowEmit is the runtime-canonical op name for `core.show.emit@1`.
const OpShowEmit = "show.emit"

// execShowEmit implements `show.emit`. Config: `topic` (string, required —
// the seed marks it required; an empty/absent topic is logged and the
// injection skipped, but `then` still fires, never a crash). Data input:
// `payload` (optional JSON; absent → JSON null). Out pin: `then`
// (immediate). No `error` pin (Blue#73).
func execShowEmit(s *Scene, t *execTask, node *ExecNode, _ string) execOpOutcome {
	topic := configString(node.Config, "topic")
	if topic == "" {
		// `topic` is required by the seed; a missing one is an authoring
		// gap surfaced at validation, not a runtime crash. Skip the
		// injection and fall through to `then` (construction-safe).
		s.logger.Warn("show.emit without topic — injection skipped", "node", node.ID)
		return execOpOutcome{}
	}
	payload, ok := s.pullData(t, node, "payload")
	if !ok || len(payload) == 0 {
		payload = json.RawMessage(`null`)
	}
	if s.emitEvent != nil {
		s.emitEvent(topic, payload)
	} else {
		// Unwired seam (no active scene reachable / pre-wiring): the
		// emission is dropped, but the chain is never amputated — `then`
		// fires (Blue#73: no error pin, construction-safe).
		s.logger.Debug("show.emit on unwired scene — no active target", "node", node.ID, "topic", topic)
	}
	// Empty outcome → applyOutcome defaults to pushing the `then` target.
	return execOpOutcome{}
}
