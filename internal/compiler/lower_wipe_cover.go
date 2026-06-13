package compiler

import "encoding/json"

// WipeCoverKind is the authoring element kind the M10 `wipe-cover` overlay is
// authored as in a Canvas/LSML scene tree (ADR 003 Amendment 5 §A5.3). It is
// recognised ONLY at lowering time and replaced by the lowered render node; it
// never reaches the served bundle as a `wipe-cover` node.
const WipeCoverKind = "wipe-cover"

// wipeCoverFill is the opaque cover fill. It mirrors Solar's
// `DEFAULT_COVER_FILL` (Solar/src/overlay/wipe-cover.ts) — a franc magenta so
// the cover at the opaque plateau is unmistakably OUR engine's paint (the M10
// probe asserts MID == magenta). An authored scene may override it via the
// element's `fill` prop; the lowering honours that, falling back to this
// default exactly as `buildWipeCoverNode` does.
const wipeCoverFill = "#C81E5A"

// lowerWipeCover lowers a `wipe-cover` AUTHORING element into the lowered render
// node Solar paints: a full-screen opaque `frame` carrying a `RenderNode`
// `keyframes` block keyed on the declared `scene_control` leaf path. The emitted
// node is BYTE-SHAPE IDENTICAL to Solar's `buildWipeCoverNode` (the parity
// oracle, ADR 003 §A5.3) — the keyframe geometry has one source of truth and
// this Go path is pinned to it by the parity test.
//
// The authoring element (in the pre-lowering tree, hence in AuthoringRoot /
// EmitLSML) is:
//
//	{ "kind": "wipe-cover", "id": "wipe-cover",
//	  "props": { "leaf": "__inputs.blue.<slug>.scene_control",
//	             "reveal_ms": 400, "hold_ms": 500, "retract_ms": 400,
//	             "fill": "#C81E5A" /* optional */ } }
//
// The leaf path keys the replay (`keyframes.key`), so a `scene_control` delta
// remounts the KeyframePlayer and re-plays reveal→hold→retract (the M9 reactive
// path). The timings are authored on the element, NEVER read from the live leaf
// value (the A5.5 "leaf carries no node shape" invariant): a leaf value only
// triggers the replay, it never supplies geometry.
//
// It returns ok=false when the element is not a well-formed wipe-cover (missing
// leaf or a non-positive-int timing). The caller then leaves the node untouched
// (a pass-through `wipe-cover` node the runtime renders as nothing — the same
// "render nothing on a non-conforming overlay" stance as Solar's
// parseWipeCoverOverlay), rather than shipping a half-built keyframe block that
// would drift from the oracle. The lowering is a PURE function: it allocates a
// fresh node and never mutates its input (matching lowerRenderProps).
func lowerWipeCover(node LayoutNode) (LayoutNode, bool) {
	leaf, ok := stringProp(node.Props, "leaf")
	if !ok || leaf == "" {
		return node, false
	}
	reveal, ok := positiveIntProp(node.Props, "reveal_ms")
	if !ok {
		return node, false
	}
	hold, ok := positiveIntProp(node.Props, "hold_ms")
	if !ok {
		return node, false
	}
	retract, ok := positiveIntProp(node.Props, "retract_ms")
	if !ok {
		return node, false
	}
	fill := wipeCoverFill
	if f, ok := stringProp(node.Props, "fill"); ok && f != "" {
		fill = f
	}

	// I4 (ADR 011 §3.5): delegate the node assembly to the general
	// buildAnimationNode path — `wipe-cover` is the degenerate Animation
	// Asset (opaque-cover opacity reveal/hold/retract). wipeCoverKeyframes
	// builds the asset-equivalent keyframes (its `key` authored inline as
	// the scene_control leaf, unlike the general animation.play path whose
	// key the compiler binds); buildAnimationNode frames it. wipe-cover is
	// the DEGENERATE geometry: a full-screen self-painting opaque cover
	// (wipeCoverProps) with NO nested target child — it paints itself, it
	// does not move a dimensioned overlay (unlike the general animation.play
	// path, which wraps + nests a sized target). The emitted bytes are
	// IDENTICAL to the pre-fix node (kind/id/props/keyframes, children nil),
	// so the byte-pin + the M10 magenta probe stay green. id defaults to
	// "wipe-cover" inside buildAnimationNode.
	out := buildAnimationNode(node.ID, wipeCoverProps(fill), wipeCoverKeyframes(leaf, reveal, hold, retract), nil)
	return out, true
}

// wipeCoverProps is the frame's static props: a full-screen opaque cover. Size
// is static (never animated — off the GPU/layout path); `background` paints the
// opaque fill (Solar's frame.tsx reads resolved.background). Mirrors
// buildWipeCoverNode's `props` block exactly.
func wipeCoverProps(fill string) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"width":      json.RawMessage(`"100%"`),
		"height":     json.RawMessage(`"100%"`),
		"background": mustMarshal(fill),
	}
}

// wipeCoverKeyframes builds the RenderNode.keyframes block byte-shape identical
// to Solar's buildWipeCoverNode (Solar/src/overlay/wipe-cover.ts):
//
//	key         = the scene_control leaf path (the reactive replay trigger)
//	duration_ms = reveal + hold + retract
//	easing      = "ease-in-out"
//	steps       = [ {at:0,         opacity:0},
//	                {at:reveal/total,        opacity:1},
//	                {at:(reveal+hold)/total, opacity:1},
//	                {at:1,         opacity:0} ]
//
// The `at` boundaries are the same float64 ratios the TS oracle computes
// (reveal_ms/total, (reveal_ms+hold_ms)/total). Both runtimes use IEEE-754
// float64, so the decoded numbers match — the parity test compares decoded
// values (the runtime contract), and these divisions are deterministic. total
// is > 0 (each timing is a positive int), so the divisions are finite.
func wipeCoverKeyframes(leaf string, reveal, hold, retract int) json.RawMessage {
	total := reveal + hold + retract
	revealAt := float64(reveal) / float64(total)
	holdEndAt := float64(reveal+hold) / float64(total)

	kf := map[string]any{
		"key":         leaf,
		"duration_ms": total,
		"easing":      "ease-in-out",
		"steps": []map[string]any{
			{"at": 0, "opacity": 0},
			{"at": revealAt, "opacity": 1},
			{"at": holdEndAt, "opacity": 1},
			{"at": 1, "opacity": 0},
		},
	}
	return mustMarshal(kf)
}

// stringProp decodes a string-typed prop. Returns ("", false) when absent or
// not a JSON string.
func stringProp(props map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := props[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// positiveIntProp decodes a strictly-positive integer prop. A non-number, a
// non-integer, or a value <= 0 yields (0, false) — matching the contract's
// isPositiveInt guard (Solar parseWipeCoverOverlay), so a malformed timing
// makes the whole element fall through rather than emit a broken keyframe.
func positiveIntProp(props map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := props[key]
	if !ok {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	if f <= 0 || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

// mustMarshal marshals a JSON-safe value or panics. Used only for values the
// lowering itself constructs from validated primitives (strings, ints, the
// fixed keyframe shape), so marshalling cannot realistically fail.
func mustMarshal(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
