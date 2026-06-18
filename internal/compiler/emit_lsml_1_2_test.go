package compiler

import (
	"encoding/json"
	"reflect"
	"testing"
)

// layout12 builds a realistic LSML 1.2 authoring tree that exercises the
// four additive 1.2 families ADR 002 §3.2 promotes to core: a per-node
// `blendMode`, a typed `mask` (image source), an image-fill in `fills[]`,
// and a `gradientTransform`. They are authored as ordinary static props on
// the node, exactly as @lumencast/compiler forwards them — Orion treats
// them as opaque json.RawMessage and must round-trip them without loss.
func layout12() LayoutNode {
	return LayoutNode{
		Kind: "frame",
		ID:   "root",
		Props: map[string]json.RawMessage{
			"size": json.RawMessage(`{"w":1920,"h":1080}`),
		},
		Children: []LayoutNode{
			{
				Kind: "shape",
				ID:   "ruby",
				Props: map[string]json.RawMessage{
					"geometry":  json.RawMessage(`"rect"`),
					"blendMode": json.RawMessage(`"hard-light"`),
					"mask":      json.RawMessage(`{"source":{"kind":"image","src":"https://cdn.example.com/ellipse.png"},"type":"alpha","op":"intersect"}`),
					"fills":     json.RawMessage(`[{"kind":"image","src":"https://cdn.example.com/tile.png","objectFit":"cover","transform":[1,0,0,1,12,4]},{"kind":"linear-gradient","stops":[{"color":"#fff","at":0}],"transform":[0.5,0,0,0.5,0,0]}]`),
				},
			},
		},
	}
}

// allowedHosts is the bundle-level asset block whose `allowedHosts` arms
// the runtime double-gate in Solar (Bastion T1/T6). It also carries a font
// with weight/style to probe the typed-model drop documented in EmitLSML.
func assets12() json.RawMessage {
	return json.RawMessage(`{"allowedHosts":["cdn.example.com","assets.zab.gg"],"preload":["https://cdn.example.com/tile.png"],"fonts":[{"family":"DIN Pro","url":"https://cdn.example.com/dinpro.woff2","sha256":"` + "0000000000000000000000000000000000000000000000000000000000000000" + `"}]}`)
}

// TestEmitLSML_1_2_RoundTrip is the #G / T6 acceptance round-trip. It
// proves, on a realistic 1.2 bundle, that Orion's emit:
//   - declares lsml "1.2" (no 1.1 downgrade),
//   - preserves assets.allowedHosts byte-identically in→out (T6),
//   - never strips the opaque 1.2 node props (blendMode/mask/fills image),
//   - fabricates no host (a no-assets push emits no allowlist).
func TestEmitLSML_1_2_RoundTrip(t *testing.T) {
	bundle, _, _, err := EmitLSML("scene-12", layout12(), nil, nil, nil, assets12())
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}

	// (1) Version is 1.2, not a 1.1 downgrade.
	if bundle.LSML != "1.2" {
		t.Fatalf("lsml = %q, want \"1.2\"", bundle.LSML)
	}

	// (2) T6 — assets.allowedHosts survives verbatim, in order, exact.
	if bundle.Assets == nil {
		t.Fatal("T6 VIOLATION: assets block dropped entirely (host allowlist would never reach the runtime gate)")
	}
	wantHosts := []string{"cdn.example.com", "assets.zab.gg"}
	if !reflect.DeepEqual(bundle.Assets.AllowedHosts, wantHosts) {
		t.Fatalf("T6 VIOLATION: allowedHosts = %v, want %v (must be preserved exactly, no strip/reorder/fabricate)",
			bundle.Assets.AllowedHosts, wantHosts)
	}
	// The block round-trips through the serialised bundle the same way it
	// is persisted + served (lsml_get.go serves these bytes opaque).
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	var reparsed struct {
		Assets struct {
			AllowedHosts []string `json:"allowedHosts"`
			Preload      []string `json:"preload"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(raw, &reparsed); err != nil {
		t.Fatalf("reparse served bundle: %v", err)
	}
	if !reflect.DeepEqual(reparsed.Assets.AllowedHosts, wantHosts) {
		t.Fatalf("T6 VIOLATION: served assets.allowedHosts = %v, want %v",
			reparsed.Assets.AllowedHosts, wantHosts)
	}
	if !reflect.DeepEqual(reparsed.Assets.Preload, []string{"https://cdn.example.com/tile.png"}) {
		t.Fatalf("assets.preload not preserved: %v", reparsed.Assets.Preload)
	}

	// (3) The 1.2 opaque node props are NOT dropped — blendMode, mask
	//     (with its image source), and the image-fill all survive on the
	//     shape node spread at top level.
	shape := lsmlFindNode(t, bundle.Layout, "ruby")
	if shape["blendMode"] != "hard-light" {
		t.Fatalf("blendMode dropped/altered: %v", shape["blendMode"])
	}
	mask, ok := shape["mask"].(map[string]any)
	if !ok {
		t.Fatalf("mask dropped or wrong type: %v", shape["mask"])
	}
	src, _ := mask["source"].(map[string]any)
	if src["kind"] != "image" || src["src"] != "https://cdn.example.com/ellipse.png" {
		t.Fatalf("mask.source not preserved: %v", mask["source"])
	}
	fills, ok := shape["fills"].([]any)
	if !ok || len(fills) != 2 {
		t.Fatalf("fills dropped or wrong arity: %v", shape["fills"])
	}
	imgFill, _ := fills[0].(map[string]any)
	if imgFill["kind"] != "image" || imgFill["objectFit"] != "cover" {
		t.Fatalf("image-fill not preserved: %v", fills[0])
	}
	if tf, _ := imgFill["transform"].([]any); len(tf) != 6 {
		t.Fatalf("image-fill gradientTransform not preserved: %v", imgFill["transform"])
	}
	gradFill, _ := fills[1].(map[string]any)
	if gradFill["kind"] != "linear-gradient" {
		t.Fatalf("gradient fill kind altered: %v", fills[1])
	}
	if gtf, _ := gradFill["transform"].([]any); len(gtf) != 6 {
		t.Fatalf("gradient transform not preserved: %v", gradFill["transform"])
	}

	// (4) No fabrication: an identical layout pushed WITHOUT an assets
	//     block emits NO assets block — Orion invents no host (T6: it
	//     "fabricates no value"), keeping deny-by-default armed downstream.
	noAssets, _, _, err := EmitLSML("scene-12", layout12(), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("EmitLSML (no assets): %v", err)
	}
	if noAssets.Assets != nil {
		t.Fatalf("T6 VIOLATION: Orion fabricated an assets block from nothing: %+v", noAssets.Assets)
	}
}

// TestEmitLSML_1_2_RetroCompat11 proves a 1.1-shaped authoring tree (one
// that uses NONE of the 1.2 constructs) still emits a structurally valid
// bundle: the layout round-trips unchanged, no assets block is invented,
// and the static-props-vs-bind split is intact. 1.2 is a pure superset, so
// the bump never perturbs a legacy scene's structure (ADR 002 §1).
func TestEmitLSML_1_2_RetroCompat11(t *testing.T) {
	bundle, _, _, err := EmitLSML("scene-1", representativeLayout(), representativeInputs(), nil, nil, nil)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}
	if bundle.Assets != nil {
		t.Fatalf("legacy push must emit no assets block, got %+v", bundle.Assets)
	}
	// The legacy layout tree is untouched: known primitive catalog, no
	// 1.2 props leaked in, bind/static split preserved.
	var root map[string]any
	if err := json.Unmarshal(bundle.Layout, &root); err != nil {
		t.Fatalf("layout not JSON: %v", err)
	}
	assertKnownPrimitiveTree(t, "layout", root)
	if _, ok := root["blendMode"]; ok {
		t.Fatal("retro-compat: 1.2 prop leaked onto a legacy node")
	}
	col := firstChild(t, root)
	title := firstChild(t, col)
	bind, ok := title["bind"].(map[string]any)
	if !ok || bind["value"] != "scene.title" {
		t.Fatalf("legacy bind split broken: %v", title["bind"])
	}
}

// TestEmitLSML_1_2_AssetsNeverSilentlyDropped guards the T6 failure mode:
// a non-empty but malformed assets block is a HARD emit error, never a
// silent drop — silently swallowing it is exactly the regression that
// would leave Solar's gate with no allowlist.
func TestEmitLSML_1_2_AssetsNeverSilentlyDropped(t *testing.T) {
	// Malformed JSON for the assets block.
	_, _, _, err := EmitLSML("scene-12", layout12(), nil, nil, nil, json.RawMessage(`{"allowedHosts": [`))
	if err == nil {
		t.Fatal("T6: a malformed assets block must error, not be silently dropped")
	}

	// An empty object carries no allowlist to preserve → emit no block
	// (byte-identical to a no-assets push), but this is NOT an error.
	bundle, _, _, err := EmitLSML("scene-12", layout12(), nil, nil, nil, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("empty assets object must not error: %v", err)
	}
	if bundle.Assets != nil {
		t.Fatalf("empty assets object must emit no block, got %+v", bundle.Assets)
	}
}
