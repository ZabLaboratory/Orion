package compiler

import (
	"bytes"
	"encoding/json"
)

// animateFromKey is the key the authored `animate` directive's mount-play
// from-state travels under inside LayoutNode.Transitions. Canvas folds the
// whole `animate` map into `transitions` on push, so `from` arrives here
// verbatim (Keeper's served-bundle dump, Pulsar runbook
// m10-animate-initial-contract-hole) — the ingest already preserves it; only
// the lowering below promotes it to the flat field the runtime reads.
const animateFromKey = "from"

// animateState mirrors the LSML 1.1 §6 animate state shape
// (@lumencast/compiler LSMLAnimateState) as it appears in `animate.from`:
//
//	{ "opacity": 0, "transform": { "scale": 0.85 | [sx,sy],
//	                               "rotate": deg, "translate": [x,y] } }
//
// Fields are RawMessage / pointers so absence is distinguishable from zero
// and a non-number value is skipped (mirroring the TS `typeof === "number"`
// guards) instead of failing the whole state.
type animateState struct {
	Opacity   json.RawMessage   `json:"opacity,omitempty"`
	Transform *animateTransform `json:"transform,omitempty"`
}

type animateTransform struct {
	Scale     json.RawMessage `json:"scale,omitempty"` // scalar or [sx, sy]
	Rotate    json.RawMessage `json:"rotate,omitempty"`
	Translate []float64       `json:"translate,omitempty"`
}

// lowerAnimateInitial promotes a node's `transitions.from` state to the flat
// `RenderNode.animate_initial` map the Lumencast runtime (≥0.3.0) feeds to
// framer-motion's `initial=` (mount-play). It is the byte-level Go mirror of
// @lumencast/compiler@0.3.0's lowerAnimateState (compile.ts:240-255), the
// parity oracle for the render-bundle ↔ runtime contract:
//
//	opacity             → opacity   (numbers only)
//	transform.scale     → scale     (scalar, or [sx,sy] collapsed to sx)
//	transform.rotate    → rotate    (numbers only)
//	transform.translate → x, y      ([x,y] pair)
//
// Keys are emitted in the oracle's insertion order (opacity, scale, rotate,
// x, y) and numbers via encoding/json's ES6-compatible float formatting, so
// the bytes match what the TS compiler's JSON.stringify emits for the same
// state. nil is returned — and the field omitted — when there is no `from`,
// when it does not decode as an animate state, or when it yields zero keys
// (the TS `Object.keys(initial).length > 0` guard): rétro-compat, the prior
// no-mount-play behaviour is untouched.
//
// PURE: reads its input, never mutates it; `transitions` (including its
// `from` entry) is left exactly as ingested — the runtime reads the timing
// off it.
func lowerAnimateInitial(transitions map[string]json.RawMessage) json.RawMessage {
	raw, ok := transitions[animateFromKey]
	if !ok {
		return nil
	}
	var s animateState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	writeNum := func(key string, f float64) {
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('"')
		buf.WriteString(key)
		buf.WriteString(`":`)
		// encoding/json formats float64 the same way ES6 JSON.stringify
		// does (shortest round-trip form) — the byte-parity anchor.
		n, _ := json.Marshal(f)
		buf.Write(n)
	}

	if f, ok := numberValue(s.Opacity); ok {
		writeNum("opacity", f)
	}
	if t := s.Transform; t != nil {
		if len(t.Scale) > 0 {
			// Scalar applies uniformly; a [sx, sy] pair collapses to sx
			// (framer takes a single scale — compile.ts:246).
			if f, ok := numberValue(t.Scale); ok {
				writeNum("scale", f)
			} else {
				var pair []float64
				if err := json.Unmarshal(t.Scale, &pair); err == nil && len(pair) > 0 {
					writeNum("scale", pair[0])
				}
			}
		}
		if f, ok := numberValue(t.Rotate); ok {
			writeNum("rotate", f)
		}
		if len(t.Translate) >= 2 {
			writeNum("x", t.Translate[0])
			writeNum("y", t.Translate[1])
		}
	}

	if buf.Len() == 1 {
		// Zero keys produced — omit the field entirely (oracle guard).
		return nil
	}
	buf.WriteByte('}')
	return json.RawMessage(buf.Bytes())
}

// numberValue decodes a RawMessage that must be a JSON number, mirroring the
// TS `typeof s.opacity === "number"` guards: absent or non-number → (0, false).
func numberValue(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	return f, true
}
