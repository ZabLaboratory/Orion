package compiler

import (
	"bytes"
	"encoding/json"
)

// This file lowers the authored LSML `animate` directive — which Canvas
// folds verbatim into LayoutNode.Transitions on push (the same ingest
// envelope lowerAnimateInitial reads its `from` out of) — into the
// PER-PROP transitions map the Lumencast runtime (≥0.3.0) actually
// consumes:
//
//	Record<prop, {kind:"tween", duration_ms:N, ease?:...} | {kind:"spring", ...}>
//
// It is the byte-level Go mirror of @lumencast/compiler@0.3.0's
// compileAnimate + the prop fan-out at compile.ts:199-217 (the parity
// oracle, executed 2026-06-09 for the reference bytes below):
//
//	envelope {from, opacity, transform, transition:{duration:1200,
//	          easing:"ease-out"}}
//	→ {"opacity":{"kind":"tween","duration_ms":1200,"ease":"cubic-out"},
//	   "scale":{"kind":"tween","duration_ms":1200,"ease":"cubic-out"}}
//
// Without this lowering the runtime's transitionFor(<prop>) finds no
// entry under the animated prop keys (the envelope keys are
// from/opacity/transform/transition, not opacity/scale/rotate/x/y) and
// the mount-play falls back to DEFAULT_MOUNT_PLAY_TRANSITION (400 ms)
// instead of the authored timing — the M10 1200 ms ramp hole.
//
// NOTE the runtime contract field is `ease` (TweenTransition,
// runtime/src/animate/transitions.ts:16-20), NOT `easing` — the oracle's
// compileAnimate emits `ease` and toFramer reads `t.ease`.

// animateEnvelope mirrors the LSML 1.1 §6 `animate` directive as it
// arrives in Transitions. Fields are RawMessage / pointers so presence
// is distinguishable from zero (the TS `!== undefined` guards at
// compile.ts:204-210 are PRESENCE checks, not type checks).
type animateEnvelope struct {
	Opacity    json.RawMessage    `json:"opacity,omitempty"`
	Transform  *envelopeTransform `json:"transform,omitempty"`
	Transition *animateTransition `json:"transition,omitempty"`
}

// envelopeTransform keeps every member raw: the fan-out only needs
// PRESENCE (compile.ts:205-210 — `transform?.scale !== undefined` etc.),
// never the values (those matter only inside `from`, handled by
// lowerAnimateInitial's animateTransform).
type envelopeTransform struct {
	Scale     json.RawMessage `json:"scale,omitempty"`
	Rotate    json.RawMessage `json:"rotate,omitempty"`
	Translate json.RawMessage `json:"translate,omitempty"`
}

// animateTransition mirrors LSMLAnimateDirective.transition.
type animateTransition struct {
	Duration  json.RawMessage `json:"duration,omitempty"`
	Easing    string          `json:"easing,omitempty"`
	Stiffness json.RawMessage `json:"stiffness,omitempty"`
	Damping   json.RawMessage `json:"damping,omitempty"`
}

// lowerTransitions returns the transitions map a lowered render node must
// carry:
//
//   - nil/empty input → returned as-is (no animate, nothing to lower).
//   - ALREADY per-prop (every value is an object carrying a string `kind`
//     — a conforming producer) → returned as-is, byte-untouched
//     (rétro-compat passthrough, non-destructive).
//   - LSML animate ENVELOPE (signature keys `transition` / `from` /
//     `transform` present) → replaced by the per-prop map per the oracle:
//     each animated prop (opacity if `opacity` present; scale/rotate from
//     `transform.*`; x AND y from `transform.translate`) gets the SAME
//     compiled transition bytes. `from` and `transition` do NOT survive
//     (`from` is promoted to animate_initial by lowerAnimateInitial). An
//     envelope with no `transition` member compiles to nil — the field is
//     omitted, exactly as the TS compiler omits `out.transitions`
//     (mount-play then uses the runtime default, the documented
//     from-without-transition behaviour).
//   - any other shape → returned as-is (unknown producer, do not destroy).
//
// PURE: never mutates its input. The result lives ONLY on the lowered
// tree (`Root`); AuthoringRoot keeps the raw envelope so EmitLSML and the
// C4 LSML content-hash are unperturbed (same stance as AnimateInitial).
func lowerTransitions(transitions map[string]json.RawMessage) map[string]json.RawMessage {
	if len(transitions) == 0 {
		return transitions
	}
	if isPerPropTransitions(transitions) {
		return transitions
	}
	if !hasAnimateEnvelopeKey(transitions) {
		return transitions
	}

	// Re-assemble the envelope from the ingest map (Canvas spread the
	// directive's members as individual Transitions entries).
	env := animateEnvelope{Opacity: transitions["opacity"]}
	if raw, ok := transitions["transform"]; ok {
		var tf envelopeTransform
		if err := json.Unmarshal(raw, &tf); err == nil {
			env.Transform = &tf
		}
	}
	if raw, ok := transitions["transition"]; ok {
		var tr animateTransition
		if err := json.Unmarshal(raw, &tr); err == nil {
			env.Transition = &tr
		}
	}

	tx := compileAnimateTransition(env.Transition)
	if tx == nil {
		return nil // compileAnimate returned undefined → no transitions field.
	}

	// Prop fan-out, oracle insertion order (compile.ts:204-210). Presence
	// checks mirror the TS `!== undefined` guards (a JSON key that exists
	// — even `null` — is "present"; an absent key is not).
	out := make(map[string]json.RawMessage, 5)
	if len(env.Opacity) > 0 {
		out["opacity"] = tx
	}
	if t := env.Transform; t != nil {
		if len(t.Scale) > 0 {
			out["scale"] = tx
		}
		if len(t.Rotate) > 0 {
			out["rotate"] = tx
		}
		if len(t.Translate) > 0 {
			out["x"] = tx
			out["y"] = tx
		}
	}
	if len(out) == 0 {
		return nil // Object.keys(transitions).length > 0 guard (compile.ts:214).
	}
	return out
}

// isPerPropTransitions reports whether the map is ALREADY in the runtime
// per-prop contract: every value is a JSON object carrying a string
// `kind` (tween/spring/crossfade/none). The LSML envelope can never
// satisfy this — its `opacity` is a number, its `from`/`transform`/
// `transition` members carry no `kind`.
func isPerPropTransitions(transitions map[string]json.RawMessage) bool {
	for _, v := range transitions {
		var probe struct {
			Kind *string `json:"kind"`
		}
		if err := json.Unmarshal(v, &probe); err != nil || probe.Kind == nil {
			return false
		}
	}
	return true
}

// hasAnimateEnvelopeKey detects the LSML animate envelope by its
// signature members. `opacity` alone is NOT a signature (it exists in
// both shapes); an unknown shape without these keys passes through
// untouched.
func hasAnimateEnvelopeKey(transitions map[string]json.RawMessage) bool {
	for _, k := range [...]string{"transition", animateFromKey, "transform"} {
		if _, ok := transitions[k]; ok {
			return true
		}
	}
	return false
}

// compileAnimateTransition is the byte-level mirror of the oracle's
// compileAnimate (compile.ts:323-344): nil when there is no `transition`
// member; a spring object when easing == "spring"; otherwise a tween
// with `duration_ms` (`t.duration ?? 200`) and the CSS→runtime easing
// map (`mapEase`, compile.ts:346-361):
//
//	linear → linear · ease-in → cubic-in · ease-out → cubic-out ·
//	ease-in-out → cubic-in-out · anything else → `ease` omitted
//
// Bytes are built in JSON.stringify insertion order (kind, duration_ms,
// ease / kind, stiffness, damping) with encoding/json's ES6-compatible
// number formatting — the byte-parity anchor shared with
// lowerAnimateInitial.
func compileAnimateTransition(t *animateTransition) json.RawMessage {
	if t == nil {
		return nil
	}
	var buf bytes.Buffer
	if t.Easing == "spring" {
		buf.WriteString(`{"kind":"spring"`)
		if len(t.Stiffness) > 0 {
			buf.WriteString(`,"stiffness":`)
			writeCompact(&buf, t.Stiffness)
		}
		if len(t.Damping) > 0 {
			buf.WriteString(`,"damping":`)
			writeCompact(&buf, t.Damping)
		}
		buf.WriteByte('}')
		return json.RawMessage(buf.Bytes())
	}
	buf.WriteString(`{"kind":"tween","duration_ms":`)
	if f, ok := numberValue(t.Duration); ok {
		n, _ := json.Marshal(f)
		buf.Write(n)
	} else {
		buf.WriteString("200") // t.duration ?? 200 (compile.ts:341)
	}
	if ease := mapEase(t.Easing); ease != "" {
		buf.WriteString(`,"ease":"`)
		buf.WriteString(ease)
		buf.WriteByte('"')
	}
	buf.WriteByte('}')
	return json.RawMessage(buf.Bytes())
}

// mapEase mirrors the oracle's CSS→runtime easing map; "" means the
// `ease` key is omitted (mapEase → undefined, dropped by JSON.stringify).
func mapEase(e string) string {
	switch e {
	case "linear":
		return "linear"
	case "ease-in":
		return "cubic-in"
	case "ease-out":
		return "cubic-out"
	case "ease-in-out":
		return "cubic-in-out"
	default:
		return ""
	}
}

// writeCompact appends a RawMessage with inter-token whitespace stripped
// (what JSON.stringify emits for the same value).
func writeCompact(buf *bytes.Buffer, raw json.RawMessage) {
	var c bytes.Buffer
	if err := json.Compact(&c, raw); err != nil {
		buf.Write(raw)
		return
	}
	buf.Write(c.Bytes())
}
