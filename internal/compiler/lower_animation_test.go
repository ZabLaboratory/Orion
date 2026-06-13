package compiler

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// I2 (ADR 011 §3.3/§3.4) — `core.animation.play@1` lowering tests: the
// `animation` authoring element resolves its `animation_id` against the
// inlined asset catalogue and lowers to a keyframed `frame` render node
// keyed on the SCALAR generation leaf `__anim.<overlay>` (§3.2). The Go
// node is asserted byte-shape identical to the Solar oracle
// `buildAnimationNode` (the parity twin, §3.3 / D6) — the extension of the
// wipe-cover parity invariant to the general path.
//
// GEOMETRY (I7 live-bug fix). The lowered node is NOT a full-screen aplat: a
// transform on a 1920×1080 uniform fill is invisible. It is a TRANSFORM
// WRAPPER dimensioned to the resolved target overlay (its `size`/position
// preserved), with the target NESTED beneath as a child so it inherits the
// animated transform/opacity. The wrapper carries no `background`. The tests
// below pin that shape (wrapper sized to target, target nested at origin,
// size preserved, no full-bleed) and prove it stays byte-parity with the
// Solar oracle.

// fadeAnimationID is the catalogue key the test asset is addressed by.
const fadeAnimationID = "fade-in"

// fadeAssetKeyframes is an authored two-step opacity fade (the asset
// geometry as ZabCanvas would inline it — NO `key`, the compiler binds it).
// Distinct from wipe-cover's 4-step reveal/hold/retract: this exercises the
// GENERAL path, not the wipe-cover specialisation.
func fadeAssetKeyframes() json.RawMessage {
	return json.RawMessage(`{"duration_ms":500,"easing":"ease-out",` +
		`"steps":[{"at":0,"opacity":0},{"at":1,"opacity":1}]}`)
}

// fadeCatalogueJSON is the inlined `animations` map (I1 shape):
// {animation_id: {target, keyframes}}.
func fadeCatalogueJSON() json.RawMessage {
	return json.RawMessage(`{"` + fadeAnimationID + `":{"target":"title",` +
		`"keyframes":` + string(fadeAssetKeyframes()) + `}}`)
}

// animationLayout authors a scene whose render content is an `animation`
// element naming the fade asset on overlay "ov", with the catalogue inlined.
// The asset's `target` ("title") is a real overlay node in the tree carrying
// its own geometry — the animation lowering resolves it, dimensions the
// wrapper to it, and NESTS it (the I7 fix). The static sibling is pruned.
func animationLayout(version string) *CanvasLayout {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	return &CanvasLayout{
		Version:    version,
		Animations: fadeCatalogueJSON(),
		Root: LayoutNode{
			Kind: "frame", ID: "root",
			Props: map[string]json.RawMessage{
				"size":       raw(`{"w":1920,"h":1080}`),
				"background": raw(`"#000000"`),
			},
			Children: []LayoutNode{
				{
					Kind: AnimationKind, ID: "anim-1",
					Props: map[string]json.RawMessage{
						"animation_id": raw(`"` + fadeAnimationID + `"`),
						"overlay_id":   raw(`"ov"`),
					},
				},
				// the target overlay the animation animates: a dimensioned
				// box with its own geometry/fill (NOT full-screen).
				{
					Kind: "text", ID: "title",
					Props: map[string]json.RawMessage{
						"x":      raw(`80`),
						"y":      raw(`360`),
						"width":  raw(`160`),
						"height": raw(`160`),
						"value":  raw(`"hello"`),
					},
				},
			},
		},
	}
}

func compileAnimation(t *testing.T, layout *CanvasLayout) *RenderBundle {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": layout},
		blueprints: map[string]*BlueprintGraph{},
		components: map[ComponentRef]*UserComponent{},
		manifest:   pureManifest(),
	}
	_, bundle, _, err := Compile(context.Background(), "scene-anim",
		PushEnvelope{CanvasVersion: "v1"}, f)
	if err != nil {
		t.Fatalf("compile animation scene: %v", err)
	}
	return bundle
}

// titleTarget is the resolved target overlay the fade asset animates — a
// dimensioned box (NOT full-screen) carrying its own geometry/fill. The
// animation lowering wraps it: the wrapper takes its `x`/`y`/`width`/`height`,
// the target is nested beneath with its position stripped (the wrapper owns
// position now), keeping its size/value.
func titleTarget() LayoutNode {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	return LayoutNode{
		Kind: "text", ID: "title",
		Props: map[string]json.RawMessage{
			"x":      raw(`80`),
			"y":      raw(`360`),
			"width":  raw(`160`),
			"height": raw(`160`),
			"value":  raw(`"hello"`),
		},
	}
}

// oracleAnimationNode is the canonical Animation Asset RenderNode shape after
// the I7 geometry fix, replicated from
// Solar/src/overlay/animation.ts::buildAnimationNode (the parity oracle, ADR
// 011 §3.3 / D6). It is the SINGLE source of truth for the node shape;
// Orion's lowering output must decode-equal this. The node is a TRANSFORM
// WRAPPER dimensioned to the target (`x`/`y`/`width`/`height`, NO background),
// keyed on the compile-bound leaf, with the target NESTED beneath at the
// wrapper origin (its `x`/`y` stripped, `size`/`value` kept).
func oracleAnimationNode(id, leaf string) map[string]any {
	return map[string]any{
		"kind": "frame",
		"id":   id,
		"props": map[string]any{
			"x":      float64(80),
			"y":      float64(360),
			"width":  float64(160),
			"height": float64(160),
		},
		"keyframes": map[string]any{
			"key":         leaf,
			"duration_ms": float64(500),
			"easing":      "ease-out",
			"steps": []any{
				map[string]any{"at": float64(0), "opacity": float64(0)},
				map[string]any{"at": float64(1), "opacity": float64(1)},
			},
		},
		"children": []any{
			map[string]any{
				"kind": "text",
				"id":   "title",
				// position stripped (wrapper owns it); size/value kept.
				"props": map[string]any{
					"width":  float64(160),
					"height": float64(160),
					"value":  "hello",
				},
			},
		},
	}
}

// TestLowerAnimation_ParityWithBuildAnimationNode (ADR 011 §6 criterion #3):
// the node lowerAnimationAsset emits is shape-identical (decoded) to Solar's
// buildAnimationNode for the same asset — the general extension of the
// wipe-cover parity invariant, now carrying the I7 wrapper+nested-target
// geometry. If the geometry, key-binding, props or nesting drift from the
// oracle, this fails loudly.
func TestLowerAnimation_ParityWithBuildAnimationNode(t *testing.T) {
	catalogue := parseAnimationCatalogue(fadeCatalogueJSON())
	node := LayoutNode{
		Kind: AnimationKind, ID: "anim-1",
		Props: map[string]json.RawMessage{
			"animation_id": json.RawMessage(`"` + fadeAnimationID + `"`),
			"overlay_id":   json.RawMessage(`"ov"`),
		},
	}
	target := titleTarget()
	lowered, ok := lowerAnimationAsset(node, catalogue, &target)
	if !ok {
		t.Fatal("lowerAnimationAsset rejected a well-formed animation element")
	}

	gotRaw, err := json.Marshal(lowered)
	if err != nil {
		t.Fatalf("marshal lowered node: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatalf("decode lowered node: %v", err)
	}

	// id = the authoring node id; leaf = __anim.<overlay_id>.
	want := oracleAnimationNode("anim-1", "__anim.ov")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lowered node diverges from buildAnimationNode oracle:\n got=%v\nwant=%v", got, want)
	}

	// The wrapper must NOT be a full-screen aplat (the I7 regression): no
	// 100% size, no background — it is a transparent transform host sized to
	// the target box.
	props := got["props"].(map[string]any)
	if props["width"] == "100%" || props["height"] == "100%" {
		t.Fatalf("wrapper is full-bleed (I7 regression): %v", props)
	}
	if _, hasBG := props["background"]; hasBG {
		t.Fatalf("transform wrapper must carry no background (it is transparent): %v", props)
	}
}

// TestLowerAnimation_TargetNestedAndSized proves the I7 geometry fix directly:
// the wrapper is sized to the target box, the target is nested beneath it (so
// it inherits the animated transform), and the nested target KEEPS its size
// but loses its absolute position (the wrapper owns it — no double-offset).
func TestLowerAnimation_TargetNestedAndSized(t *testing.T) {
	catalogue := parseAnimationCatalogue(fadeCatalogueJSON())
	node := LayoutNode{
		Kind: AnimationKind, ID: "anim-1",
		Props: map[string]json.RawMessage{
			"animation_id": json.RawMessage(`"` + fadeAnimationID + `"`),
			"overlay_id":   json.RawMessage(`"ov"`),
		},
	}
	target := titleTarget()
	lowered, ok := lowerAnimationAsset(node, catalogue, &target)
	if !ok {
		t.Fatal("lowerAnimationAsset rejected a well-formed animation element")
	}

	// wrapper sized/positioned to the target box.
	for k, want := range map[string]string{"x": "80", "y": "360", "width": "160", "height": "160"} {
		if got := string(lowered.Props[k]); got != want {
			t.Fatalf("wrapper prop %q = %q, want target geometry %q", k, got, want)
		}
	}

	// exactly one nested child — the target overlay.
	if len(lowered.Children) != 1 {
		t.Fatalf("wrapper must nest the target as its single child; got %d children", len(lowered.Children))
	}
	child := lowered.Children[0]
	if child.ID != "title" {
		t.Fatalf("nested child id = %q, want the target overlay %q", child.ID, "title")
	}
	// size preserved on the nested target...
	for k, want := range map[string]string{"width": "160", "height": "160"} {
		if got := string(child.Props[k]); got != want {
			t.Fatalf("nested target lost its %q: got %q, want %q", k, got, want)
		}
	}
	// ...but position stripped (the wrapper owns it; keeping it double-offsets).
	for _, k := range []string{"x", "y"} {
		if _, kept := child.Props[k]; kept {
			t.Fatalf("nested target kept %q — would double-offset inside the wrapper", k)
		}
	}
}

// TestLowerAnimation_ServedBundle: compiling a scene with an `animation`
// element yields a served RenderBundle whose tree contains the keyframed
// node — lowered to a `frame`, keyed on the SCALAR leaf __anim.<overlay>
// (criterion #2: scalar leaf on the wire), with the authoring `animation`
// props gone.
func TestLowerAnimation_ServedBundle(t *testing.T) {
	bundle := compileAnimation(t, animationLayout("v1"))

	node := findNode(bundle.Root, "anim-1")
	if node == nil {
		t.Fatal("served bundle: animation node not found under root")
	}
	if node.Kind != "frame" {
		t.Fatalf("served animation node kind = %q, want lowered to %q", node.Kind, "frame")
	}
	if len(node.Keyframes) == 0 {
		t.Fatal("served animation node carries no keyframes block")
	}
	for _, k := range []string{"animation_id", "overlay_id"} {
		if _, leaked := node.Props[k]; leaked {
			t.Fatalf("authoring prop %q leaked into served frame props: %v", k, keysOf(node.Props))
		}
	}

	var kf struct {
		Key        string `json:"key"`
		DurationMS int    `json:"duration_ms"`
		Easing     string `json:"easing"`
		Steps      []struct {
			At      float64 `json:"at"`
			Opacity float64 `json:"opacity"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(node.Keyframes, &kf); err != nil {
		t.Fatalf("decode keyframes: %v", err)
	}
	if kf.Key != "__anim.ov" {
		t.Fatalf("keyframes.key = %q, want the scalar generation leaf %q", kf.Key, "__anim.ov")
	}
	if kf.DurationMS != 500 || kf.Easing != "ease-out" || len(kf.Steps) != 2 {
		t.Fatalf("keyframes geometry not the authored asset: %+v", kf)
	}

	// The target overlay ("title") is NESTED under the animation wrapper, so
	// it must NOT also appear as a static sibling (the I7 pruning — otherwise
	// the overlay renders twice, once static once animated). It is reachable
	// only THROUGH the wrapper.
	if dup := findNodeAmongSiblings(bundle.Root, "anim-1", "title"); dup {
		t.Fatal("target overlay still present as a static sibling — must be pruned (nested under the wrapper only)")
	}
	if len(node.Children) != 1 || node.Children[0].ID != "title" {
		t.Fatalf("animation wrapper must nest the target overlay; children=%v", node.Children)
	}
}

// findNodeAmongSiblings reports whether a node with id `target` exists in the
// tree at a position that is NOT a descendant of the node with id `parent`.
// Used to assert the animation target was pruned from its original sibling
// location (it now lives only nested under the wrapper).
func findNodeAmongSiblings(root LayoutNode, parent, target string) bool {
	var walk func(LayoutNode, bool) bool
	walk = func(n LayoutNode, underParent bool) bool {
		if !underParent && n.ID == target {
			return true
		}
		next := underParent || n.ID == parent
		for _, c := range n.Children {
			if walk(c, next) {
				return true
			}
		}
		return false
	}
	return walk(root, false)
}

// TestLowerAnimation_FallsThroughInert: an `animation` element that resolves
// no asset (unknown id, missing target, absent/empty catalogue) — or whose
// target node is absent from the layout (nothing to wrap/move) — is NOT
// lowered. It stays a pass-through node the runtime renders as nothing (§3.4,
// the same inert stance as a non-conforming wipe-cover). No half-built
// keyframe ships, and no full-screen aplat fallback is emitted (the I7 fix:
// a targetless animation has nothing to move).
func TestLowerAnimation_FallsThroughInert(t *testing.T) {
	catalogue := parseAnimationCatalogue(fadeCatalogueJSON())
	missingTarget := parseAnimationCatalogue(json.RawMessage(
		`{"x":{"target":"","keyframes":{"duration_ms":1,"easing":"e","steps":[{"at":0},{"at":1}]}}}`))
	target := titleTarget()

	cases := map[string]struct {
		node   LayoutNode
		cat    map[string]animationAsset
		target *LayoutNode
	}{
		"unknown animation_id": {
			node: LayoutNode{Kind: AnimationKind, ID: "a", Props: map[string]json.RawMessage{
				"animation_id": json.RawMessage(`"nope"`), "overlay_id": json.RawMessage(`"ov"`)}},
			cat: catalogue, target: &target,
		},
		"missing animation_id": {
			node: LayoutNode{Kind: AnimationKind, ID: "a", Props: map[string]json.RawMessage{
				"overlay_id": json.RawMessage(`"ov"`)}},
			cat: catalogue, target: &target,
		},
		"empty catalogue": {
			node: LayoutNode{Kind: AnimationKind, ID: "a", Props: map[string]json.RawMessage{
				"animation_id": json.RawMessage(`"` + fadeAnimationID + `"`), "overlay_id": json.RawMessage(`"ov"`)}},
			cat: nil, target: &target,
		},
		"asset missing target": {
			node: LayoutNode{Kind: AnimationKind, ID: "a", Props: map[string]json.RawMessage{
				"animation_id": json.RawMessage(`"x"`), "overlay_id": json.RawMessage(`"ov"`)}},
			cat: missingTarget, target: &target,
		},
		// well-formed asset, but the target node is absent from the layout
		// (index miss) → nil target → inert, no full-screen fallback (I7).
		"target node absent from layout": {
			node: LayoutNode{Kind: AnimationKind, ID: "a", Props: map[string]json.RawMessage{
				"animation_id": json.RawMessage(`"` + fadeAnimationID + `"`), "overlay_id": json.RawMessage(`"ov"`)}},
			cat: catalogue, target: nil,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := lowerAnimationAsset(c.node, c.cat, c.target); ok {
				t.Fatal("non-conforming animation element was lowered — want inert fall-through")
			}
			out := lowerRenderTree(c.node, c.cat)
			if out.Kind != AnimationKind {
				t.Fatalf("non-conforming animation lowered to %q, want pass-through %q", out.Kind, AnimationKind)
			}
			if len(out.Keyframes) != 0 {
				t.Fatal("non-conforming animation emitted a keyframes block")
			}
		})
	}
}

// TestLowerAnimation_OverlayDefaultsToNodeID: when `overlay_id` is absent
// the generation-leaf namespace defaults to the element's node id (§3.4) —
// the authored element id IS the overlay namespace by default.
func TestLowerAnimation_OverlayDefaultsToNodeID(t *testing.T) {
	catalogue := parseAnimationCatalogue(fadeCatalogueJSON())
	node := LayoutNode{Kind: AnimationKind, ID: "scoreboard", Props: map[string]json.RawMessage{
		"animation_id": json.RawMessage(`"` + fadeAnimationID + `"`)}}
	target := titleTarget()
	lowered, ok := lowerAnimationAsset(node, catalogue, &target)
	if !ok {
		t.Fatal("lowerAnimationAsset rejected an element with default overlay")
	}
	var kf struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(lowered.Keyframes, &kf); err != nil {
		t.Fatalf("decode keyframes: %v", err)
	}
	if kf.Key != "__anim.scoreboard" {
		t.Fatalf("keyframes.key = %q, want __anim.<node-id> default %q", kf.Key, "__anim.scoreboard")
	}
}

// TestLowerAnimation_LSMLHashUnperturbed: like wipe-cover (§A5.5), authoring
// an `animation` element does NOT feed the C4 LSML content-hash — EmitLSML
// reads the AUTHORING tree where the node is still the opaque `animation`
// kind. So the keyframes lowering is render-bundle-only and adopt-on-verify
// is unperturbed.
func TestLowerAnimation_LSMLHashUnperturbed(t *testing.T) {
	bundle := compileAnimation(t, animationLayout("v1"))

	lsmlBundle, hashA, _, err := EmitLSML(
		"scene-anim", bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}
	authoring := lsmlFindNode(t, lsmlBundle.Layout, "anim-1")
	if k, _ := authoring["kind"].(string); k != AnimationKind {
		t.Fatalf("LSML node kind = %q, want authoring %q (keyframes must not reach the LSML tree)", k, AnimationKind)
	}
	if _, leaked := authoring["keyframes"]; leaked {
		t.Fatal("keyframes leaked into the LSML authoring tree — would perturb the C4 hash (A5.5 violation)")
	}
	_, hashB, _, err := EmitLSML(
		"scene-anim", bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML (2nd): %v", err)
	}
	if hashA != hashB {
		t.Fatalf("LSML hash not deterministic across emits: %q vs %q", hashA, hashB)
	}
}
