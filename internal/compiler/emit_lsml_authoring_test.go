package compiler

import (
	"encoding/json"
	"testing"
)

// TestEmitLSML_FromCompile_CarriesAuthoringVocab closes the gap Vigil
// flagged on PR #42: every existing EmitLSML test builds the root BY HAND
// (representativeLayout / a literal LayoutNode), so none exercises the
// real consumption path — Compile → EmitLSML — where the lowering sits
// between the authoring tree and what a hand-off passes to EmitLSML.
//
// The C4 contract (ADR 007 §9.6 / §C.4): the LSML 1.1 bundle MUST be in
// the AUTHORING vocab (`style.fontSize`, `geometry`, `size.{w,h}`,
// `cornerRadius`, nested `stroke`), because Prism computes its
// adopt-on-verify hash from the authoring tree (`sceneToLsml`). If Orion
// emits from the LOWERED render tree (`size`/`colour`/`width`/`kind`),
// lsml.HashBundle diverges and C4 never collapses (permanent
// LSML_HASH_MISMATCH).
//
// This test compiles the representative authoring scene and emits LSML
// from the bundle the *production* caller would use. On the FIXED code it
// reads bundle.AuthoringRoot (authoring vocab) and passes. On the BUGGY
// code (EmitLSML fed bundle.Root, lowered) every authoring-key assertion
// below fires — verified by flipping the source to bundle.Root, see the
// sibling _Root test which asserts the inverse on the lowered tree.
func TestEmitLSML_FromCompile_CarriesAuthoringVocab(t *testing.T) {
	bundle := compileAuthoring(t)

	// Emit from the SAME tree the production push path now uses
	// (scenes_push.go: EmitLSML(..., bundle.AuthoringRoot, ...)).
	lsmlBundle, _, _, err := EmitLSML(
		"scene-1", bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}

	text := lsmlFindNode(t, lsmlBundle.Layout, "label")
	// Authoring vocab present: nested `style` object survives verbatim.
	style, ok := text["style"]
	if !ok {
		t.Fatalf("text node must carry authoring `style` object, got keys %v", keysOfAny(text))
	}
	var styleObj map[string]any
	if err := json.Unmarshal(mustRaw(t, style), &styleObj); err != nil {
		t.Fatalf("style not an object: %v", err)
	}
	if _, ok := styleObj["fontSize"]; !ok {
		t.Fatalf("authoring style.fontSize must be present, style=%v", styleObj)
	}
	// Lowered render keys must NOT be there: emitting authoring vocab means
	// no flat `size`/`weight`/`colour`/`align` at the text node top level.
	for _, k := range []string{"size", "weight", "colour", "align"} {
		if _, leaked := text[k]; leaked {
			t.Fatalf("lowered render key %q leaked into LSML text node (authoring-vocab violation): %v",
				k, keysOfAny(text))
		}
	}

	frame := lsmlFindNode(t, lsmlBundle.Layout, "root")
	if _, ok := frame["size"]; !ok {
		t.Fatalf("frame must carry authoring nested `size` object, got %v", keysOfAny(frame))
	}
	for _, k := range []string{"width", "height"} {
		if _, leaked := frame[k]; leaked {
			t.Fatalf("lowered render key %q leaked into LSML frame node: %v", k, keysOfAny(frame))
		}
	}

	shape := lsmlFindNode(t, lsmlBundle.Layout, "chip")
	for _, k := range []string{"geometry", "size", "cornerRadius"} {
		if _, ok := shape[k]; !ok {
			t.Fatalf("shape must carry authoring key %q, got %v", k, keysOfAny(shape))
		}
	}
	// `kind` is the LSML STRUCTURAL field (LayoutNode.Kind == "shape"), not
	// the render `geometry→kind` rename — so it is legitimately present and
	// excluded here. The render-exclusive prop keys (width/height/radius/
	// stroke_width) are the unambiguous lowering leaks to guard against.
	if kindVal, _ := shape["kind"].(string); kindVal != "shape" {
		t.Fatalf("shape structural kind must be %q, got %q", "shape", kindVal)
	}
	for _, k := range []string{"width", "height", "radius", "stroke_width"} {
		if _, leaked := shape[k]; leaked {
			t.Fatalf("lowered render key %q leaked into LSML shape node: %v", k, keysOfAny(shape))
		}
	}
}

// TestEmitLSML_FromLoweredRoot_IsRenderVocab is the inverse witness: it
// emits from bundle.Root (the LOWERED tree EmitLSML was wrongly fed before
// the fix) and asserts the LSML then carries the RENDER vocab. This pins
// the regression — if a future change made EmitLSML lower again (or a
// caller passed bundle.Root), the authoring-vocab test above would fail
// AND this one documents exactly what the broken output looked like:
// flat `size`/`kind`/`width`, no `style`/`geometry`. It is the "verify by
// inverting" Vigil asked for, kept as a permanent guard.
func TestEmitLSML_FromLoweredRoot_IsRenderVocab(t *testing.T) {
	bundle := compileAuthoring(t)

	lsmlBundle, _, _, err := EmitLSML(
		"scene-1", bundle.Root, bundle.OperatorInputs, bundle.ExternalAdapters, nil, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}

	text := lsmlFindNode(t, lsmlBundle.Layout, "label")
	if _, ok := text["size"]; !ok {
		t.Fatalf("lowered-root emit must carry flat render `size`, got %v", keysOfAny(text))
	}
	if _, ok := text["style"]; ok {
		t.Fatalf("lowered-root emit must NOT carry authoring `style`, got %v", keysOfAny(text))
	}

	shape := lsmlFindNode(t, lsmlBundle.Layout, "chip")
	if _, ok := shape["kind"]; !ok {
		t.Fatalf("lowered-root emit must carry flat render `kind`, got %v", keysOfAny(shape))
	}
	if _, ok := shape["geometry"]; ok {
		t.Fatalf("lowered-root emit must NOT carry authoring `geometry`, got %v", keysOfAny(shape))
	}
}

// lsmlFindNode decodes the opaque LSML layout (json.RawMessage tree) and
// returns the node map with the given id, failing the test if absent.
func lsmlFindNode(t *testing.T, layout json.RawMessage, id string) map[string]any {
	t.Helper()
	var node map[string]any
	if err := json.Unmarshal(layout, &node); err != nil {
		t.Fatalf("decode lsml node: %v", err)
	}
	if got, _ := node["id"].(string); got == id {
		return node
	}
	if children, ok := node["children"].([]any); ok {
		for _, c := range children {
			cb, err := json.Marshal(c)
			if err != nil {
				t.Fatalf("re-marshal child: %v", err)
			}
			if found := lsmlFindNodeOpt(cb, id); found != nil {
				return found
			}
		}
	}
	t.Fatalf("lsml node %q not found", id)
	return nil
}

// lsmlFindNodeOpt is the non-fatal recursive helper (returns nil when the
// id is not in this subtree).
func lsmlFindNodeOpt(raw json.RawMessage, id string) map[string]any {
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil
	}
	if got, _ := node["id"].(string); got == id {
		return node
	}
	if children, ok := node["children"].([]any); ok {
		for _, c := range children {
			cb, err := json.Marshal(c)
			if err != nil {
				return nil
			}
			if found := lsmlFindNodeOpt(cb, id); found != nil {
				return found
			}
		}
	}
	return nil
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	if raw, ok := v.(json.RawMessage); ok {
		return raw
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return raw
}
