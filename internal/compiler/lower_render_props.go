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
// they pass through unchanged.
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
	default:
		// stack, grid, image, media, repeat, instance, user components:
		// no authoring→render rename is defined; pass through verbatim
		// (fresh copies so the caller never aliases our input). stack is
		// already aligned (direction/gap/align/justify/wrap/crossGap),
		// and the universal props are flat already.
		return copyProps(props), copyBindings(bindings)
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
	"fontSize":   "size",   // text.tsx:10 resolved.size
	"fontWeight": "weight", // text.tsx:11 resolved.weight
	"color":      "colour", // text.tsx:12 resolved.colour (US→GB, text only)
	"textAlign":  "align",  // text.tsx:13 resolved.align
}

// textKeep is the set of top-level text props the runtime reads as-is.
// value (bound, text.tsx:9) and opacity (text.tsx:14) pass through.
// Everything else the producer emits inside `style` that the runtime
// does NOT read (fontFamily/lineHeight/letterSpacing/…) is dropped — we
// do not fabricate render keys the `.tsx` never reads (ADR 007 §9.3).
var textKeep = map[string]struct{}{
	"value":   {},
	"opacity": {},
}

func lowerText(props map[string]json.RawMessage, bindings map[string]string) (map[string]json.RawMessage, map[string]string) {
	out := make(map[string]json.RawMessage)

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

// copyBindings returns a shallow copy of a bindings map. nil → nil.
func copyBindings(bindings map[string]string) map[string]string {
	if len(bindings) == 0 {
		return nil
	}
	out := make(map[string]string, len(bindings))
	for k, v := range bindings {
		out[k] = v
	}
	return out
}

// lowerRenderTree applies lowerRenderProps to every node in the tree,
// returning a fresh tree (the input is not mutated). It is the recursive
// driver the compile tail calls on the assembled render-bundle root.
func lowerRenderTree(node LayoutNode) LayoutNode {
	out := node
	out.Props, out.Bindings = lowerRenderProps(node.Kind, node.Props, node.Bindings)
	if len(node.Children) > 0 {
		out.Children = make([]LayoutNode, len(node.Children))
		for i, c := range node.Children {
			out.Children[i] = lowerRenderTree(c)
		}
	} else {
		out.Children = nil
	}
	return out
}
