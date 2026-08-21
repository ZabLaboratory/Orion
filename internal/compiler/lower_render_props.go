package compiler

import "encoding/json"

// lowerRenderProps translates one LayoutNode's authoring-vocab props
// (and any bindings keyed on those props) into the FLAT render vocab the
// Lumencast runtime actually reads. It is the missing LSML→RenderBundle
// lowering step (ADR 007 §9): Prism authors LSML — nested + US-spelled
// (`style:{fontSize,fontWeight,color}`, `size:{w,h}`, `geometry`,
// `cornerRadius`) — but the runtime primitives read flat keys straight
// off `resolved` (`resolveProps = {...node.props}` overlaid by bindings;
// `text.tsx` reads `resolved.size`/`weight`/`colour`, `frame.tsx` reads
// `resolved.width`/`height`, `shape.tsx` reads `resolved.kind`/`radius`,
// verified verbatim against the runtime `.tsx`). Without this step the
// scene paints at default font/size/dims (the gap found by the live
// render on 2026-06-05).
//
// It is a PURE function: it allocates fresh maps and never mutates its
// inputs. It is deterministic — the same node always lowers to the same
// shape. Direction is one-way (authoring→render); the runtime never
// round-trips back to authoring, so there is no inverse here. Round-trip
// fidelity is LSML's job, upstream of Orion (Prism to-lsml/from-lsml).
//
// `stack` and the universal props (visible/rotation/sizing/opacity, read
// flat by tree.tsx's UniversalWrapper) already match the runtime, so
// they pass through unchanged — except a nested `size:{w,h}`, which is
// split into flat width/height for EVERY kind (see the default branch).
//
// Bindings re-key (ADR 007 §9.5): `resolveProps` overlays each binding
// by its KEY onto `resolved` (`resolved[propKey] = store.get(path)`,
// tree.tsx:118-134). A binding keyed on an authoring prop name therefore
// lands on the wrong resolved key. So a binding must be renamed with the
// same map as the static props — a `bindStyle.fontSize` binding keyed
// `style.fontSize` (or the nested-object key the producer emitted) is
// re-keyed to `size`, so a *bound* font-size still resolves onto
// `resolved.size`.
func lowerRenderProps(
	kind string,
	props map[string]json.RawMessage,
	bindings map[string]string,
) (map[string]json.RawMessage, map[string]string) {
	switch kind {
	case "text":
		return lowerText(props, bindings)
	case "frame":
		return lowerFrame(props, bindings)
	case "shape":
		return lowerShape(props, bindings)
	case "image":
		return lowerImage(props, bindings)
	default:
		// stack, grid, media, repeat, instance, user components: no per-key
		// rename is defined, BUT a nested `size:{w,h}` must split into flat
		// width/height like every other primitive. Solar reads
		// `resolved.width`/`height`; a `sizing:fixed` auto-layout frame
		// (serialised to a `stack`) that reached the runtime with only a
		// nested `size` object had NO box and collapsed to 0 (the
		// canevas-chat-sponso right column / camera rail). Every other key
		// (direction/gap/align/justify/wrap/crossGap + the universal props)
		// is already flat and passes through verbatim.
		out := copyProps(props)
		if v, ok := out["size"]; ok {
			delete(out, "size")
			splitSize(v, out)
		}
		return out, rekeyBindings(bindings, sizeBindingRenames())
	}
}

// renameMap is the per-primitive authoring-key → render-key table. A key
// absent from the map is kept verbatim. Nested-object keys (style.*,
// size.{w,h}, stroke:{}) are handled separately by the splitters because
// they change the SHAPE, not just the name.
type renameMap map[string]string

// textRenames covers the flat renames inside `style`. The nested `style`
// object is flattened first (see lowerText), so these apply to the keys
// AFTER flattening. The producer emits these inside `style`.
var textRenames = renameMap{
	"fontSize":       "size",   // text.tsx resolved.size
	"fontFamily":     "font",   // text.tsx resolved.font (LSML style.fontFamily)
	"fontWeight":     "weight", // text.tsx resolved.weight
	"color":          "colour", // text.tsx resolved.colour (US→GB, text only)
	"textAlign":      "align",  // text.tsx resolved.align
	"lineHeight":     "lineHeight",
	"letterSpacing":  "letterSpacing",
	"textTransform":  "textTransform",
	"textDecoration": "textDecoration",
	"fontStyle":      "fontStyle",
}

// textContentRename lowers a text node's CONTENT key. `text` is the
// natural authoring key for a text node's displayed string (LSML text
// content; the runtime even reserves `text` as a known-but-unconsumed
// prop key in render/prop-allowlist.js). The runtime's text primitive
// however reads ONLY `resolved.value` (text.tsx). A producer that emits
// the content as `text` (static prop OR binding) would otherwise land on
// `resolved.text` — ignored — leaving `resolved.value` undefined and the
// span empty (the live "Solar paints black in mode=broadcast" incident,
// leaderboard scene 57dc631f: bundle bound `text:` → black). Lower
// `text` → `value` so an authored-as-`text` content still resolves.
// A producer that already emits `value` is unaffected (`value` is in
// textKeep and is not a rename source).
const textContentAuthoringKey = "text"
const textContentRenderKey = "value"

// textKeep is the set of top-level text props the runtime reads as-is.
// value (bound) and opacity pass through. Everything else the producer
// emits inside `style` that the runtime does NOT read (lineHeight/
// letterSpacing/…) is dropped — we do not fabricate render keys the
// `.tsx` never reads (ADR 007 §9.3).
var textKeep = map[string]struct{}{
	"value":          {},
	"opacity":        {},
	"maxLines":       {},
	"lineHeight":     {},
	"letterSpacing":  {},
	"textTransform":  {},
	"textDecoration": {},
	"fontStyle":      {},
}

func lowerText(props map[string]json.RawMessage, bindings map[string]string) (map[string]json.RawMessage, map[string]string) {
	out := make(map[string]json.RawMessage)
	// Text geometry is intentionally stored in LSML's advisory
	// metadata.figma.size (the authoring text schema has no first-class
	// size field). Solar, however, needs the flattened universal
	// width/height pair to wrap text inside its panel. Lower the metadata
	// fallback here, without trusting metadata as a runtime prop.
	lowerTextMetadataGeometry(props["metadata"], out)

	// The binding-rename table is derived from the static mapping
	// UNCONDITIONALLY — a bound prop has no static counterpart in
	// `props`, so the rename must not depend on the static key being
	// present. Both the nested `style.<k>` form and the flat `<k>` form
	// map to the render key.
	rename := make(map[string]string)
	for ak, rk := range textRenames {
		rename["style."+ak] = rk
		rename[ak] = rk
	}
	// Content key : an authored-as-`text` binding re-keys to `value`
	// (the render vocab the runtime reads). See textContentRename above.
	rename[textContentAuthoringKey] = textContentRenderKey

	for k, v := range props {
		switch k {
		case "style":
			// Flatten the nested style object, applying textRenames to
			// each inner key and dropping inner keys the runtime ignores.
			var style map[string]json.RawMessage
			if err := json.Unmarshal(v, &style); err == nil {
				for sk, sv := range style {
					if rk, ok := textRenames[sk]; ok {
						out[rk] = sv
					}
					// inner keys the runtime never reads (fontFamily, …)
					// are silently dropped — survive in LSML, no render slot.
				}
			}
		case "metadata":
			// Authoring metadata is consumed above only for text geometry and
			// truncation. It is not a Solar text prop and must not reach the
			// runtime allowlist as an ignored key.
			continue
		case "size":
			// Be tolerant of an already-materialised nested size from an
			// older producer; the canonical LSML text path uses metadata.
			splitSize(v, out)
		case textContentAuthoringKey:
			// Content authored as `text` → the render vocab `value`. A
			// node that already carries `value` keeps it (textKeep below);
			// a node carrying both is malformed authoring — `value` wins
			// only if it is processed after, so guard against clobber.
			if _, hasValue := props[textContentRenderKey]; !hasValue {
				out[textContentRenderKey] = v
			}
		default:
			if _, ok := textKeep[k]; ok {
				out[k] = v
			} else if rk, ok := textRenames[k]; ok {
				// A producer that emitted the prop flat (not nested in
				// style) — tolerate it: rename in place.
				out[rk] = v
			} else {
				// Unknown top-level key: keep it (forward-compatible; a
				// new runtime read shows up here without a code change).
				out[k] = v
			}
		}
	}
	return out, rekeyBindings(bindings, rename)
}

// lowerTextMetadataGeometry extracts only the typed numeric fields Solar
// consumes from the Figma authoring metadata. Invalid or partial metadata is
// ignored, never copied into the render bundle.
func lowerTextMetadataGeometry(metadata json.RawMessage, out map[string]json.RawMessage) {
	if len(metadata) == 0 {
		return
	}
	var envelope struct {
		Figma struct {
			Size struct {
				W json.RawMessage `json:"w"`
				H json.RawMessage `json:"h"`
			} `json:"size"`
			MaxLines json.RawMessage `json:"maxLines"`
		} `json:"figma"`
	}
	if err := json.Unmarshal(metadata, &envelope); err != nil {
		return
	}
	if _, exists := out["width"]; !exists && positiveFiniteJSONNumber(envelope.Figma.Size.W) {
		out["width"] = envelope.Figma.Size.W
	}
	if _, exists := out["height"]; !exists && positiveFiniteJSONNumber(envelope.Figma.Size.H) {
		out["height"] = envelope.Figma.Size.H
	}
	if _, exists := out["maxLines"]; !exists && positiveIntegerJSONNumber(envelope.Figma.MaxLines) {
		out["maxLines"] = envelope.Figma.MaxLines
	}
}

func positiveFiniteJSONNumber(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value > 0
}

func positiveIntegerJSONNumber(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value > 0
}

func lowerFrame(props map[string]json.RawMessage, bindings map[string]string) (map[string]json.RawMessage, map[string]string) {
	out := make(map[string]json.RawMessage)
	rename := sizeBindingRenames() // size/size.w/size.h → width/height

	for k, v := range props {
		switch k {
		case "size":
			// Split {w,h} into flat width/height (frame.tsx:19-20).
			splitSize(v, out)
		default:
			// background (frame.tsx:27 legacy fallback), x/y/opacity/
			// scale/rotate, backgrounds[] — all already flat keys the
			// runtime reads. Keep verbatim.
			out[k] = v
		}
	}
	return out, rekeyBindings(bindings, rename)
}

// shapeRenames are the flat top-level renames for a shape node.
var shapeRenames = renameMap{
	"geometry":     "kind",   // shape.tsx:22 resolved.kind (rect/circle/line)
	"cornerRadius": "radius", // shape.tsx:28 resolved.radius
}

func lowerShape(props map[string]json.RawMessage, bindings map[string]string) (map[string]json.RawMessage, map[string]string) {
	out := make(map[string]json.RawMessage)

	// Unconditional binding-rename table: size split + the flat shape
	// renames + the nested-stroke split keys.
	rename := sizeBindingRenames()
	for ak, rk := range shapeRenames {
		rename[ak] = rk
	}
	rename["stroke.color"] = "stroke"
	rename["stroke.width"] = "stroke_width"

	for k, v := range props {
		switch k {
		case "size":
			splitSize(v, out) // → width / height (shape.tsx:26-27)
		case "stroke":
			// The producer may emit either a flat string stroke colour
			// (legacy 1.0 — keep) or a nested {color,width} object —
			// split into flat stroke (colour) + stroke_width (number).
			splitStroke(v, out)
		default:
			if rk, ok := shapeRenames[k]; ok {
				out[rk] = v
			} else {
				// fill (shape.tsx:23), opacity (:29), fills[]/strokes[]
				// (:36-37), x/y — flat keys the runtime reads. Keep.
				out[k] = v
			}
		}
	}
	return out, rekeyBindings(bindings, rename)
}

// lowerImage splits the image's nested `size:{w,h}` into flat width/height
// (image.tsx reads resolved.width/height to honour intrinsic dimensions;
// absent → it fills its container). alt/fit/src/position/opacity/x/y are
// flat keys the runtime reads as-is.
func lowerImage(props map[string]json.RawMessage, bindings map[string]string) (map[string]json.RawMessage, map[string]string) {
	out := make(map[string]json.RawMessage)
	rename := sizeBindingRenames() // size/size.w/size.h → width/height
	for k, v := range props {
		switch k {
		case "size":
			splitSize(v, out) // → width / height (image.tsx)
		default:
			out[k] = v
		}
	}
	return out, rekeyBindings(bindings, rename)
}

// sizeBindingRenames is the binding-rename table for the nested `size`
// object, shared by frame and shape. The authoring forms a binding could
// target are the whole `size` (lands on width by convention) or the
// dotted `size.w`/`size.h`.
func sizeBindingRenames() map[string]string {
	return map[string]string{
		"size":   "width",
		"size.w": "width",
		"size.h": "height",
	}
}

// splitSize turns a nested {"w":..,"h":..} value into flat width/height
// keys on out. A size value that is not an object (or lacks w/h) is
// dropped — the runtime has no nested `size` slot, so passing it through
// would be dead weight.
func splitSize(v json.RawMessage, out map[string]json.RawMessage) {
	var size map[string]json.RawMessage
	if err := json.Unmarshal(v, &size); err != nil {
		return
	}
	if w, ok := size["w"]; ok {
		out["width"] = w
	}
	if h, ok := size["h"]; ok {
		out["height"] = h
	}
}

// splitStroke turns a nested {"color":..,"width":..} stroke into flat
// stroke (colour string) + stroke_width (number). A non-object stroke (a
// legacy flat colour string) is kept verbatim under `stroke`.
func splitStroke(v json.RawMessage, out map[string]json.RawMessage) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(v, &obj); err != nil {
		// Not an object — legacy flat stroke colour. Keep as-is.
		out["stroke"] = v
		return
	}
	// A real object: map color→stroke, width→stroke_width.
	if c, ok := obj["color"]; ok {
		out["stroke"] = c
	}
	if w, ok := obj["width"]; ok {
		out["stroke_width"] = w
	}
}

// rekeyBindings returns a fresh bindings map with every key that the
// rename table covers renamed to its render key; other keys pass
// through. A nil/empty input yields nil so the JSON omits the field.
func rekeyBindings(bindings map[string]string, rename map[string]string) map[string]string {
	if len(bindings) == 0 {
		return nil
	}
	out := make(map[string]string, len(bindings))
	for k, path := range bindings {
		if rk, ok := rename[k]; ok {
			out[rk] = path
		} else {
			out[k] = path
		}
	}
	return out
}

// copyProps returns a shallow copy of a props map (values are immutable
// json.RawMessage byte slices, shared safely). nil → nil so JSON omits.
func copyProps(props map[string]json.RawMessage) map[string]json.RawMessage {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(props))
	for k, v := range props {
		out[k] = v
	}
	return out
}

// lowerRenderTree applies lowerRenderProps to every node in the tree,
// returning a fresh tree (the input is not mutated). It is the recursive
// driver the compile tail calls on the assembled render-bundle root.
// `animations` is the inlined Animation Asset catalogue (ADR 011 §3.1),
// resolved when lowering an `animation` element; nil for scenes with none.
//
// It is the entry point: it first indexes every node by id so an
// `animation` element can resolve + NEST its target overlay (the I7
// geometry fix), then drives the recursive lowering. The set of target ids
// consumed by `animation` elements is collected so those nodes are PRUNED
// from their original sibling location (they now live nested under the
// animation wrapper — leaving the original would render the overlay twice,
// once static and once animated).
func lowerRenderTree(node LayoutNode, animations map[string]animationAsset) LayoutNode {
	// No catalogue → no target nesting/pruning possible; take the cheap
	// path that walks without the index (every `animation` element falls
	// through inert anyway).
	if len(animations) == 0 {
		return lowerRenderTreeRec(node, animations, nil, nil)
	}
	index := indexNodesByID(node)
	consumed := collectAnimationTargets(node, animations)
	return lowerRenderTreeRec(node, animations, index, consumed)
}

// indexNodesByID builds a flat id→node map of the layout tree so an
// `animation` element can resolve its `asset.target` to the actual target
// LayoutNode (its geometry is the source of truth the wrapper is sized to).
// A later duplicate id is ignored (first wins) — authored ids are expected
// unique; the index is read-only.
func indexNodesByID(node LayoutNode) map[string]LayoutNode {
	out := make(map[string]LayoutNode)
	var walk func(LayoutNode)
	walk = func(n LayoutNode) {
		if n.ID != "" {
			if _, exists := out[n.ID]; !exists {
				out[n.ID] = n
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(node)
	return out
}

// collectAnimationTargets returns the set of layout-node ids that a
// conforming `animation` element resolves as its target (and will nest).
// Those ids are pruned from their original location so the overlay is not
// rendered twice. Only well-formed elements (resolvable animation_id +
// non-empty asset target) contribute — a non-conforming element nests
// nothing, so prunes nothing.
func collectAnimationTargets(node LayoutNode, animations map[string]animationAsset) map[string]struct{} {
	out := make(map[string]struct{})
	var walk func(LayoutNode)
	walk = func(n LayoutNode) {
		if n.Kind == AnimationKind {
			if animID, ok := stringProp(n.Props, "animation_id"); ok && animID != "" {
				if asset, ok := animations[animID]; ok && asset.Target != "" {
					out[asset.Target] = struct{}{}
				}
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(node)
	return out
}

// lowerRenderTreeRec is the recursive lowering driver. `index` resolves an
// `animation` element's target overlay (nil when no catalogue); `consumed`
// is the set of target ids to prune from their sibling position (they are
// nested under the animation wrapper). Both are nil on the no-catalogue
// fast path.
func lowerRenderTreeRec(node LayoutNode, animations map[string]animationAsset, index map[string]LayoutNode, consumed map[string]struct{}) LayoutNode {
	// The `wipe-cover` authoring element lowers to a keyframed `frame` render
	// node (ADR 003 Amendment 5 §A5.3): a different kind + props + a synthesised
	// `keyframes` block, so it is handled before the generic per-kind prop
	// lowering. A malformed element falls through (ok=false) to the generic
	// path, where it stays an inert `wipe-cover` node the runtime renders as
	// nothing (no half-built keyframe shipped). The keyframes block lives ONLY
	// on this lowered tree (`Root`); the pre-lowering authoring node keeps
	// `kind:"wipe-cover"` for EmitLSML, so the C4 LSML hash is unperturbed.
	if node.Kind == WipeCoverKind {
		if lowered, ok := lowerWipeCover(node); ok {
			// wipe-cover is a leaf overlay node — no children to recurse into.
			lowered.Children = nil
			return lowered
		}
	}

	// The `animation` authoring element (ADR 011 §3.3/§3.4) lowers to the same
	// keyframed `frame` render node via the general lower_animation.go path:
	// it resolves its `animation_id` against the inlined asset catalogue and
	// keys the replay on the SCALAR generation leaf `__anim.<overlay>` (§3.2).
	// Same inert-fallthrough + LSML-hash-unperturbed discipline as wipe-cover:
	// a missing asset / target / catalogue leaves the node a pass-through the
	// runtime renders as nothing, and the keyframes block rides ONLY this
	// lowered tree (the authoring node keeps `kind:"animation"` for EmitLSML).
	if node.Kind == AnimationKind {
		var target *LayoutNode
		// Resolve the target overlay from the tree index and lower it to
		// render vocab BEFORE nesting it, so the nested overlay paints with
		// its real geometry/fill (the same lowering every other node gets).
		// The recursion into the lowered target carries the same index so a
		// nested target may itself contain further animation elements.
		if index != nil {
			if animID, ok := stringProp(node.Props, "animation_id"); ok && animID != "" {
				if asset, ok := animations[animID]; ok && asset.Target != "" {
					if raw, ok := index[asset.Target]; ok {
						lt := lowerRenderTreeRec(raw, animations, index, consumed)
						target = &lt
					}
				}
			}
		}
		if lowered, ok := lowerAnimationAsset(node, animations, target); ok {
			return lowered
		}
	}

	out := node
	out.Props, out.Bindings = lowerRenderProps(node.Kind, node.Props, node.Bindings)
	// Mount-play (LSML 1.1 §6 `animate.from`): promote the from-state riding
	// inside `transitions` to the flat `animate_initial` field the runtime
	// reads (render-bundle ↔ runtime contract, Pulsar runbook
	// m10-animate-initial-contract-hole). Lives ONLY on this lowered tree —
	// AuthoringRoot/EmitLSML never see it, so the C4 LSML hash is unperturbed
	// (same stance as Keyframes).
	if initial := lowerAnimateInitial(node.Transitions); initial != nil {
		out.AnimateInitial = initial
	}
	// Timing (LSML 1.1 §6 `animate.transition`): replace the raw ingest
	// envelope with the PER-PROP transitions map the runtime's
	// transitionFor(<prop>) reads (lower_transitions.go — the other half of
	// the same contract: without it the authored duration is lost and the
	// mount-play falls back to the 400 ms default). Already-per-prop or
	// unknown shapes pass through untouched; AuthoringRoot keeps the raw
	// envelope, so EmitLSML / the C4 hash are unperturbed.
	out.Transitions = lowerTransitions(node.Transitions)
	if len(node.Children) > 0 {
		lowered := make([]LayoutNode, 0, len(node.Children))
		for _, c := range node.Children {
			// Prune children consumed as an animation target: they are
			// nested under the animation wrapper (lowerAnimationAsset), so
			// leaving them here would render the overlay twice — once
			// static (this sibling), once animated (under the wrapper).
			if consumed != nil && c.ID != "" {
				if _, isTarget := consumed[c.ID]; isTarget {
					continue
				}
			}
			lowered = append(lowered, lowerRenderTreeRec(c, animations, index, consumed))
		}
		if len(lowered) > 0 {
			out.Children = lowered
		} else {
			out.Children = nil
		}
	} else {
		out.Children = nil
	}
	return out
}
