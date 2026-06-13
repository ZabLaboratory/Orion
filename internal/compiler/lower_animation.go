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
//	asset.target  → the TARGET layout node the keyframes animate (§3.4),
//	                resolved from the layout tree by id
//	asset.keyframes + the generation-leaf path `__anim.<overlay>` (§3.2)
//	              → a `frame` RenderNode carrying the keyframe block keyed
//	                on that SCALAR leaf — replayed by Solar's KeyframePlayer
//	                on every `animation.play` firing (the M9 reactive path).
//
// GEOMETRY (I7 live-bug fix). The lowered keyframe node is NOT a full-screen
// aplat: a `translateX`/`opacity` animation on a 1920×1080 uniform fill is
// invisible (nothing moves, the whole frame fades together). The lowered
// node is instead a TRANSFORM WRAPPER **dimensioned to the target overlay**
// (its authored `size`/position preserved), with the target overlay node
// NESTED as its child so it inherits the animated `transform`/`opacity`
// (Solar's KeyframePlayer applies the keyframe channels to the wrapper box,
// which composites onto its subtree — see Solar render/keyframe-player.tsx).
// The target overlay keeps its own geometry; the wrapper only frames + moves
// it. `wipe-cover` is the degenerate case (no target overlay): a full-screen
// self-painting opaque cover with no nested child — see lowerWipeCover.
//
// The wrapper-node shape both `animation.play` and `wipe-cover` emit has ONE
// source of truth (D6): the Solar oracle `buildAnimationNode`
// (Solar/src/overlay/animation.ts), pinned by the Go↔TS parity test.

// buildAnimationNode assembles the lowered keyframe `frame` RenderNode —
// the SINGLE source of truth for the node shape both `animation.play` and
// `wipe-cover` emit, and the Go half of the Go↔TS parity oracle (its TS
// twin is Solar's `buildAnimationNode`, Solar/src/overlay/animation.ts).
//
// It is intentionally minimal and PURE: a `frame` carrying the supplied
// `props` (geometry/fill — a dimensioned box for an `animation.play` target
// wrapper, or the full-screen cover for `wipe-cover`), the supplied
// `keyframes` block verbatim (the animation source of truth), and the
// supplied `children` nested beneath (the target overlay for
// `animation.play`, nil for the self-painting `wipe-cover` cover). id
// defaults to the wipe-cover-compatible "wipe-cover" when empty (so the
// delegated wipe-cover node keeps its byte-identical id).
func buildAnimationNode(id string, props map[string]json.RawMessage, keyframes json.RawMessage, children []LayoutNode) LayoutNode {
	if id == "" {
		id = WipeCoverKind
	}
	return LayoutNode{
		Kind:      "frame",
		ID:        id,
		Props:     props,
		Keyframes: keyframes,
		Children:  children,
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
// `animation_id` against the scene's authored animation catalogue and its
// `asset.target` against the layout tree (the TARGET overlay node, supplied
// already render-lowered).
//
// Resolution is compile-time and inert-on-failure (the same stance as a
// non-conforming `wipe-cover`, §3.4): ok=false — leaving the node a
// pass-through the runtime renders as nothing — when
//   - `animation_id` is absent/empty, or
//   - it names no asset in the catalogue, or
//   - the asset's `target` is missing, or
//   - the target node is absent from the layout (target == nil), or
//   - the catalogue itself is malformed/empty.
//
// The leaf the replay keys on is `__anim.<overlay>` (§3.2), with `overlay`
// = the element's `overlay_id` prop or (fallback) its node id. The asset's
// authored `keyframes.key` is OVERRIDDEN with this compile-bound leaf
// (§3.3: the trigger leaf is a compiler concern, never authored for the
// general path).
//
// The emitted node is a TRANSFORM WRAPPER dimensioned to the target
// overlay (its `size`/position copied from the resolved target props),
// with the target NESTED beneath it so it inherits the animated transform
// (the I7 geometry fix). It is a PURE function: it allocates a fresh node
// and never mutates its inputs.
func lowerAnimationAsset(node LayoutNode, catalogue map[string]animationAsset, target *LayoutNode) (LayoutNode, bool) {
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
	// The target overlay must exist in the layout — its geometry is the
	// source of truth the wrapper is dimensioned to. A missing target
	// node falls through inert (no full-screen aplat fallback: a
	// targetless animation has nothing to move).
	if target == nil {
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

	id := node.ID
	if id == "" {
		id = overlay
	}

	// The wrapper is positioned/sized to the target so the animated
	// transform moves a DIMENSIONED box, not a full-screen fill. The target
	// node is nested beneath at the wrapper's ORIGIN (its `x`/`y` stripped —
	// the wrapper owns the position now; leaving them would double-offset
	// the overlay inside the wrapper). The nested target keeps its own
	// `size`/fill and paints its content; the wrapper only frames + animates
	// it. The wrapper carries NO `background` — it is a transparent
	// transform host.
	wProps, child := wrapperAndChildProps(*target)
	out := buildAnimationNode(id, wProps, keyframes, []LayoutNode{child})
	return out, true
}

// wrapperAndChildProps splits a resolved target overlay node into (a) the
// transform-wrapper frame's props — the target's position (`x`/`y`) and
// size (`width`/`height`), so the animated box is dimensioned to the
// overlay rather than the full screen — and (b) a fresh copy of the target
// with its `x`/`y` stripped, so it renders at the wrapper's ORIGIN (the
// wrapper already positions the box; keeping the target's absolute coords
// would offset it twice). The wrapper carries no `background`/`fill`; the
// nested target keeps its own paint. A target without explicit position
// yields a wrapper sized only by `width`/`height` and an unmodified child.
func wrapperAndChildProps(target LayoutNode) (map[string]json.RawMessage, LayoutNode) {
	wrapper := make(map[string]json.RawMessage)
	for _, k := range []string{"x", "y", "width", "height"} {
		if v, ok := target.Props[k]; ok {
			wrapper[k] = v
		}
	}

	child := target
	child.Props = make(map[string]json.RawMessage, len(target.Props))
	for k, v := range target.Props {
		if k == "x" || k == "y" {
			continue // the wrapper owns position; the child renders at origin
		}
		child.Props[k] = v
	}
	if len(child.Props) == 0 {
		child.Props = nil
	}
	return wrapper, child
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
