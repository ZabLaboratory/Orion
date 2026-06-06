package compiler

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// authoringLayout builds a representative scene in the AUTHORING vocab
// Prism's producer emits (ADR 007 §9): a frame (size:{w,h}, background)
// wrapping a stack (already-aligned flat props) that holds a text node
// (style:{fontSize,fontWeight,color,textAlign}, bound value) and a shape
// (geometry, size:{w,h}, fill, cornerRadius, nested stroke). The runtime
// reads NONE of these nested/US-spelled keys — it reads the flat render
// vocab (size/weight/colour, width/height, kind/radius). This is exactly
// the bundle the live render served at default font/size/dims.
func authoringLayout(version string) *CanvasLayout {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	return &CanvasLayout{
		Version: version,
		Root: LayoutNode{
			Kind: "frame",
			ID:   "root",
			Props: map[string]json.RawMessage{
				"size":       raw(`{"w":400,"h":120}`),
				"background": raw(`"#101010"`),
			},
			Children: []LayoutNode{
				{
					Kind: "stack",
					ID:   "col",
					Props: map[string]json.RawMessage{
						"direction": raw(`"vertical"`),
						"gap":       raw(`8`),
						"align":     raw(`"center"`),
						"justify":   raw(`"flex-start"`),
					},
					Children: []LayoutNode{
						{
							Kind: "text",
							ID:   "label",
							Props: map[string]json.RawMessage{
								"style": raw(`{"fontSize":48,"fontWeight":700,"color":"#fff","textAlign":"center","fontFamily":"Inter"}`),
							},
							// value comes from state — the only thing that
							// rendered today (bound, kept verbatim).
							Bindings: map[string]string{"value": "score.team_a"},
						},
						{
							Kind: "shape",
							ID:   "chip",
							Props: map[string]json.RawMessage{
								"geometry":     raw(`"rect"`),
								"size":         raw(`{"w":200,"h":80}`),
								"fill":         raw(`"#0af"`),
								"cornerRadius": raw(`8`),
								"stroke":       raw(`{"color":"#222","width":2}`),
							},
						},
						{
							Kind: "image",
							ID:   "logo",
							Props: map[string]json.RawMessage{
								"alt":  raw(`"logo"`),
								"size": raw(`{"w":96,"h":64}`),
								"fit":  raw(`"contain"`),
								"src":  raw(`"http://x/logo.svg"`),
							},
						},
					},
				},
			},
		},
	}
}

// findNode walks the lowered RenderBundle root for a node by id.
func findNode(n LayoutNode, id string) *LayoutNode {
	if n.ID == id {
		return &n
	}
	for i := range n.Children {
		if got := findNode(n.Children[i], id); got != nil {
			return got
		}
	}
	return nil
}

func compileAuthoring(t *testing.T) *RenderBundle {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": authoringLayout("v1")},
		blueprints: map[string]*BlueprintGraph{},
		components: map[ComponentRef]*UserComponent{},
		manifest:   pureManifest(),
	}
	// Blueprint-free layout-only scene (the default live case).
	_, bundle, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1"}, f)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	return bundle
}

// jsonEq asserts a node prop unmarshals to the wanted Go value.
func jsonEq(t *testing.T, node *LayoutNode, key string, want any) {
	t.Helper()
	raw, ok := node.Props[key]
	if !ok {
		t.Fatalf("node %q: prop %q absent (props: %v)", node.ID, key, keysOf(node.Props))
	}
	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("node %q prop %q: unmarshal %s: %v", node.ID, key, raw, err)
	}
	// Numbers decode as float64; normalise the wanted side.
	if wf, ok := normNum(want); ok {
		want = wf
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("node %q prop %q = %v (%T), want %v (%T)", node.ID, key, got, got, want, want)
	}
}

func normNum(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func absent(t *testing.T, node *LayoutNode, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := node.Props[k]; ok {
			t.Fatalf("node %q: authoring key %q must be gone after lowering (props: %v)",
				node.ID, k, keysOf(node.Props))
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestLowerRenderProps_Bundle is the proving test (ADR 007 §9.5). It
// FAILS on the pre-fix pass-through compiler — the served RenderBundle
// then carries the nested authoring keys (style/size/geometry/
// cornerRadius) and lacks the flat render keys (size/weight/colour/
// width/height/kind/radius), so every assertion below fires. After the
// lowering lands at the compile tail, all pass.
func TestLowerRenderProps_Bundle(t *testing.T) {
	bundle := compileAuthoring(t)
	root := bundle.Root

	// --- text: style.{fontSize,fontWeight,color,textAlign} → flat ---
	text := findNode(root, "label")
	if text == nil {
		t.Fatal("text node 'label' not found in bundle")
	}
	jsonEq(t, text, "size", 48)
	jsonEq(t, text, "weight", 700)
	jsonEq(t, text, "colour", "#fff")
	jsonEq(t, text, "align", "center")
	jsonEq(t, text, "font", "Inter") // style.fontFamily → font (text.tsx reads resolved.font)
	// authoring keys gone (style flattened + renamed away).
	absent(t, text, "style", "fontSize", "fontWeight", "color", "textAlign", "fontFamily")
	// bound value survives, re-keyed unchanged (value stays value).
	if got, ok := text.Bindings["value"]; !ok || got != "score.team_a" {
		t.Fatalf("text binding value: got %q ok=%v, want score.team_a", got, ok)
	}

	// --- frame: size.{w,h} → width/height; background kept ---
	frame := findNode(root, "root")
	if frame == nil {
		t.Fatal("frame node 'root' not found")
	}
	jsonEq(t, frame, "width", 400)
	jsonEq(t, frame, "height", 120)
	jsonEq(t, frame, "background", "#101010")
	absent(t, frame, "size")

	// --- shape: geometry→kind, size→width/height, cornerRadius→radius,
	//     nested stroke → stroke + stroke_width; fill kept ---
	shape := findNode(root, "chip")
	if shape == nil {
		t.Fatal("shape node 'chip' not found")
	}
	jsonEq(t, shape, "kind", "rect")
	jsonEq(t, shape, "width", 200)
	jsonEq(t, shape, "height", 80)
	jsonEq(t, shape, "radius", 8)
	jsonEq(t, shape, "fill", "#0af")
	jsonEq(t, shape, "stroke", "#222")
	jsonEq(t, shape, "stroke_width", 2)
	absent(t, shape, "geometry", "size", "cornerRadius")

	// --- image: size.{w,h} → width/height; alt/fit/src kept ---
	image := findNode(root, "logo")
	if image == nil {
		t.Fatal("image node 'logo' not found")
	}
	jsonEq(t, image, "width", 96)
	jsonEq(t, image, "height", 64)
	jsonEq(t, image, "fit", "contain")
	jsonEq(t, image, "src", "http://x/logo.svg")
	absent(t, image, "size")

	// --- stack: NO regression — props byte-identical to authoring ---
	stack := findNode(root, "col")
	if stack == nil {
		t.Fatal("stack node 'col' not found")
	}
	want := authoringLayout("v1").Root.Children[0].Props
	if !reflect.DeepEqual(stack.Props, want) {
		t.Fatalf("stack props changed by lowering: got %v want %v", stack.Props, want)
	}
}

// TestLowerRenderProps_Pure asserts the function does not mutate its
// inputs (determinism + purity, ADR 007 §9.5).
func TestLowerRenderProps_Pure(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	props := map[string]json.RawMessage{"style": raw(`{"fontSize":24,"color":"#abc"}`)}
	bindings := map[string]string{"value": "x.y"}

	// Snapshot the inputs.
	before, _ := json.Marshal(props)
	beforeB := map[string]string{}
	for k, v := range bindings {
		beforeB[k] = v
	}

	outP, outB := lowerRenderProps("text", props, bindings)

	// Inputs unchanged.
	after, _ := json.Marshal(props)
	if string(before) != string(after) {
		t.Fatalf("lowerRenderProps mutated props: %s → %s", before, after)
	}
	if !reflect.DeepEqual(bindings, beforeB) {
		t.Fatalf("lowerRenderProps mutated bindings: %v", bindings)
	}

	// Output is the lowered shape.
	if _, ok := outP["size"]; !ok {
		t.Fatalf("expected lowered size key, got %v", keysOf(outP))
	}
	if _, ok := outP["colour"]; !ok {
		t.Fatalf("expected lowered colour key, got %v", keysOf(outP))
	}
	_ = outB

	// Determinism: a second call yields an equal result.
	outP2, _ := lowerRenderProps("text", props, bindings)
	if !reflect.DeepEqual(outP, outP2) {
		t.Fatalf("non-deterministic: %v != %v", outP, outP2)
	}
}

// TestLowerRenderProps_BoundFontSize proves a binding keyed on an
// authoring prop is re-keyed to the render key so a BOUND font-size
// still lands on resolved.size (ADR 007 §9.5 bindings clause).
func TestLowerRenderProps_BoundFontSize(t *testing.T) {
	props := map[string]json.RawMessage{}
	bindings := map[string]string{"style.fontSize": "ui.titleSize", "value": "score"}
	_, outB := lowerRenderProps("text", props, bindings)

	if got, ok := outB["size"]; !ok || got != "ui.titleSize" {
		t.Fatalf("bound font-size: outB[size]=%q ok=%v, want ui.titleSize (outB=%v)", got, ok, outB)
	}
	if _, stale := outB["style.fontSize"]; stale {
		t.Fatalf("authoring binding key style.fontSize must be re-keyed away (outB=%v)", outB)
	}
	if got := outB["value"]; got != "score" {
		t.Fatalf("value binding must pass through, got %q", got)
	}
}
