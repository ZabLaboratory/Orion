package compiler

import "encoding/json"

// ADR 011 (I2) — `core.animation.play@1` lowering onto the proven Path-A
// KeyframePlayer mechanism.
//
// An **Animation Asset** (authored, inlined in the layout's `animations`
// catalogue, ADR 011 §3.1 / I1) declares a `target` layout-node id and a
// parameterised `keyframes` block (`{key?, duration_ms, easing, steps[]}`,
// the exact shape `wipe-cover` already emits). `lowerAnimationAsset`
// resolves, at compile time:
//
//	animation_id  → the authored asset (catalogue lookup)
//	asset.target  → the layout node the keyframes animate (§3.4)
//	asset.keyframes + the generation-leaf path `__anim.<overlay>` (§3.2)
//	              → a `frame` RenderNode carrying the keyframe block keyed
//	                on that SCALAR leaf — replayed by Solar's KeyframePlayer
//	                on every `animation.play` firing (the M9 reactive path).
//
// The emitted node is BYTE-SHAPE IDENTICAL to the Solar oracle
// `buildAnimationNode` (the parity twin of `buildWipeCoverNode`), so the
// keyframe geometry has ONE source of truth (D6) — the Go↔TS parity test
// pins it. `wipe-cover` is the degenerate case: `lowerWipeCover` builds the
// reveal/hold/retract keyframes and routes through `buildAnimationNode`,
// duplicating no geometry (§3.5 / I4).
//
// Per §3.3 the `key` is BOUND by the compiler (to `__anim.<overlay>`),
// never read off the wire; an asset MAY author its own `key` (wipe-cover
// does — its `scene_control` leaf), in which case the authored key is kept
// (the wipe-cover byte-pin). The general `animation.play` asset leaves
// `key` for the compiler to bind. `params` substitution (static overrides
// baked at compile, §3.3) is deferred to a follow-up (Risk R3); v1 lowers
// the asset's authored geometry verbatim.

// buildAnimationNode assembles the lowered keyframe `frame` RenderNode —
// the SINGLE source of truth for the node shape both `animation.play` and
// `wipe-cover` emit, and the Go half of the Go↔TS parity oracle (its TS
// twin is Solar's `buildAnimationNode`, Solar/src/overlay/animation.ts).
//
// It is intentionally minimal and PURE: a full-screen opaque `frame` whose
// `background` is `fill`, carrying the supplied `keyframes` block verbatim.
// The keyframes block is the geometry source of truth — this function only
// frames it. id defaults to the wipe-cover-compatible "wipe-cover" when
// empty (so the delegated wipe-cover node keeps its byte-identical id).
func buildAnimationNode(id, fill string, keyframes json.RawMessage) LayoutNode {
	if id == "" {
		id = WipeCoverKind
	}
	return LayoutNode{
		Kind:      "frame",
		ID:        id,
		Props:     wipeCoverProps(fill),
		Keyframes: keyframes,
	}
}

// AnimationKind is the authoring element kind that names a play of an
// Animation Asset in the layout tree (ADR 011 §3.3/§3.4). Like
// `wipe-cover` it is recognised ONLY at lowering time and replaced by the
// lowered keyframe node; it never reaches the served bundle as an
// `animation` node.
//
// Authoring element (pre-lowering tree):
//
//	{ "kind": "animation", "id": "<overlay_id>",
//	  "props": { "animation_id": "<catalogue key>",
//	             "overlay_id": "<overlay namespace, optional>",
//	             "fill": "#…" /* optional */ } }
//
// `overlay_id` names the generation-leaf namespace `__anim.<overlay_id>`
// (§3.4); when absent it defaults to the node id, so the authored element
// id IS the overlay namespace by default — symmetric with how the exec op
// derives the leaf from `overlay_id`.
const AnimationKind = "animation"

// lowerAnimationAsset lowers an `animation` AUTHORING element into the
// keyframed `frame` render node (ADR 011 §3.3/§3.4), resolving its
// `animation_id` against the scene's authored animation catalogue.
//
// Resolution is compile-time and inert-on-failure (the same stance as a
// non-conforming `wipe-cover`, §3.4): ok=false — leaving the node a
// pass-through the runtime renders as nothing — when
//   - `animation_id` is absent/empty, or
//   - it names no asset in the catalogue, or
//   - the asset's `target` is missing, or
//   - the catalogue itself is malformed/empty.
//
// The leaf the replay keys on is `__anim.<overlay>` (§3.2), with `overlay`
// = the element's `overlay_id` prop or (fallback) its node id. The asset's
// authored `keyframes.key` is OVERRIDDEN with this compile-bound leaf
// (§3.3: the trigger leaf is a compiler concern, never authored for the
// general path). It is a PURE function: it allocates a fresh node and never
// mutates its input.
func lowerAnimationAsset(node LayoutNode, catalogue map[string]animationAsset) (LayoutNode, bool) {
	if len(catalogue) == 0 {
		return node, false
	}
	animID, ok := stringProp(node.Props, "animation_id")
	if !ok || animID == "" {
		return node, false
	}
	asset, ok := catalogue[animID]
	if !ok || asset.Target == "" || len(asset.Keyframes) == 0 {
		return node, false
	}

	overlay, ok := stringProp(node.Props, "overlay_id")
	if !ok || overlay == "" {
		overlay = node.ID
	}
	if overlay == "" {
		return node, false
	}

	// Bind the replay trigger leaf to the scalar generation leaf (§3.2/§3.4):
	// override whatever `key` (if any) the asset authored — the general
	// `animation.play` trigger is the compiler's to assign.
	keyframes, ok := withKey(asset.Keyframes, "__anim."+overlay)
	if !ok {
		return node, false
	}

	fill := wipeCoverFill
	if f, ok := stringProp(node.Props, "fill"); ok && f != "" {
		fill = f
	}

	id := node.ID
	if id == "" {
		id = overlay
	}
	out := buildAnimationNode(id, fill, keyframes)
	return out, true
}

// animationAsset is the compile-time view of one inlined Animation Asset
// (ADR 011 §3.1 / I1): a `target` layout-node id and an opaque `keyframes`
// block. The keyframes are carried as raw JSON — Orion is a transport for
// the keyframe shape whose single source of truth is the authoring schema
// (ZabCanvas animation_asset.py) + the Solar oracle; the compiler binds the
// `key` and frames it, it does not re-interpret the steps.
type animationAsset struct {
	Target    string          `json:"target"`
	Keyframes json.RawMessage `json:"keyframes"`
}

// parseAnimationCatalogue decodes the layout's inlined `animations` map
// (ADR 011 §3.1 / I1: `{animation_id: {target, keyframes{…}}}`). A
// nil/empty/malformed catalogue yields an empty map — the lowering then
// falls through inert for every `animation` element (§3.4). Decode errors
// are swallowed into "no catalogue": a malformed authored asset renders
// nothing, never a broken keyframe (the inert-fallthrough doctrine).
func parseAnimationCatalogue(raw json.RawMessage) map[string]animationAsset {
	if len(raw) == 0 {
		return nil
	}
	var cat map[string]animationAsset
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil
	}
	return cat
}

// withKey overrides the `key` field of an asset's keyframes block with the
// compile-bound leaf path (ADR 011 §3.3), preserving every other field
// (duration_ms, easing, steps, plus any extra) verbatim. It decodes to a
// generic object so the steps' authored scalar values ride through
// untouched — Orion does not re-interpret them. ok=false when the
// keyframes block is not a JSON object (a malformed asset → inert
// fallthrough). The re-marshalled bytes are deterministic (encoding/json
// sorts object keys), so the emitted node is stable across pushes.
func withKey(keyframes json.RawMessage, leaf string) (json.RawMessage, bool) {
	var kf map[string]json.RawMessage
	if err := json.Unmarshal(keyframes, &kf); err != nil || kf == nil {
		return nil, false
	}
	keyRaw, err := json.Marshal(leaf)
	if err != nil {
		return nil, false
	}
	kf["key"] = keyRaw
	out, err := json.Marshal(kf)
	if err != nil {
		return nil, false
	}
	return out, true
}
