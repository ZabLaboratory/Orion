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

// oracleAnimationNode is the canonical Animation Asset RenderNode shape,
// replicated from Solar/src/overlay/animation.ts::buildAnimationNode (the
// parity oracle, ADR 011 §3.3 / D6). It is the SINGLE source of truth for
// the node shape; Orion's lowering output must decode-equal this. The
// `key` is the compile-bound leaf, the `steps` are the asset's authored
// geometry verbatim.
func oracleAnimationNode(id, leaf, fill string) map[string]any {
	return map[string]any{
		"kind": "frame",
		"id":   id,
		"props": map[string]any{
			"width":      "100%",
			"height":     "100%",
			"background": fill,
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
	}
}

// TestLowerAnimation_ParityWithBuildAnimationNode (ADR 011 §6 criterion #3):
// the node lowerAnimationAsset emits is shape-identical (decoded) to Solar's
// buildAnimationNode for the same asset — the general extension of the
// wipe-cover parity invariant. If the geometry, key-binding or props drift
// from the oracle, this fails loudly.
func TestLowerAnimation_ParityWithBuildAnimationNode(t *testing.T) {
	catalogue := parseAnimationCatalogue(fadeCatalogueJSON())
	node := LayoutNode{
		Kind: AnimationKind, ID: "anim-1",
		Props: map[string]json.RawMessage{
			"animation_id": json.RawMessage(`"` + fadeAnimationID + `"`),
			"overlay_id":   json.RawMessage(`"ov"`),
		},
	}
	lowered, ok := lowerAnimationAsset(node, catalogue)
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

	// id = the authoring node id; leaf = __anim.<overlay_id>; fill = default.
	want := oracleAnimationNode("anim-1", "__anim.ov", "#C81E5A")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lowered node diverges from buildAnimationNode oracle:\n got=%v\nwant=%v", got, want)
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
}

// TestLowerAnimation_FallsThroughInert: an `animation` element that resolves
// no asset (unknown id, missing target, absent/empty catalogue) is NOT
// lowered — it stays a pass-through node the runtime renders as nothing
// (§3.4, the same inert stance as a non-conforming wipe-cover). No
// half-built keyframe ships.
func TestLowerAnimation_FallsThroughInert(t *testing.T) {
	catalogue := parseAnimationCatalogue(fadeCatalogueJSON())
	missingTarget := parseAnimationCatalogue(json.RawMessage(
		`{"x":{"target":"","keyframes":{"duration_ms":1,"easing":"e","steps":[{"at":0},{"at":1}]}}}`))

	cases := map[string]struct {
		node LayoutNode
		cat  map[string]animationAsset
	}{
		"unknown animation_id": {
			node: LayoutNode{Kind: AnimationKind, ID: "a", Props: map[string]json.RawMessage{
				"animation_id": json.RawMessage(`"nope"`), "overlay_id": json.RawMessage(`"ov"`)}},
			cat: catalogue,
		},
		"missing animation_id": {
			node: LayoutNode{Kind: AnimationKind, ID: "a", Props: map[string]json.RawMessage{
				"overlay_id": json.RawMessage(`"ov"`)}},
			cat: catalogue,
		},
		"empty catalogue": {
			node: LayoutNode{Kind: AnimationKind, ID: "a", Props: map[string]json.RawMessage{
				"animation_id": json.RawMessage(`"` + fadeAnimationID + `"`), "overlay_id": json.RawMessage(`"ov"`)}},
			cat: nil,
		},
		"asset missing target": {
			node: LayoutNode{Kind: AnimationKind, ID: "a", Props: map[string]json.RawMessage{
				"animation_id": json.RawMessage(`"x"`), "overlay_id": json.RawMessage(`"ov"`)}},
			cat: missingTarget,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := lowerAnimationAsset(c.node, c.cat); ok {
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
	lowered, ok := lowerAnimationAsset(node, catalogue)
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
