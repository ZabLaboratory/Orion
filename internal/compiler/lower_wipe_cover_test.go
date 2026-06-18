package compiler

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// m10LeafPath is the scene_control leaf the M10 scene declares and the
// wipe-cover overlay keys its replay on (pinned by the scene_control contract
// + the m10-orion-scene fixture, ADR 003 §A4.2 / §A5.7 RC (a)).
const m10LeafPath = "__inputs.blue.m10-scene-control.scene_control"

// m10WipeCoverLayout authors the M10 Orion scene: a full-screen root whose
// render content is the `wipe-cover` overlay element keyed on the declared
// scene_control leaf, above an (unrendered) marker. It mirrors the fixture
// scripts/fixtures/m10-orion-scene.lsml.json (the 400/500/400 timings, the
// magenta cover) but as an Orion CanvasLayout (the compile entry shape).
func m10WipeCoverLayout(version string) *CanvasLayout {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	return &CanvasLayout{
		Version: version,
		Inputs: []OperatorInput{
			{
				Path:  m10LeafPath,
				Label: "M10 scene-control",
				Type:  "json",
			},
		},
		Root: LayoutNode{
			Kind: "frame",
			ID:   "root",
			Props: map[string]json.RawMessage{
				"size":       raw(`{"w":1920,"h":1080}`),
				"background": raw(`"#000000"`),
			},
			Children: []LayoutNode{
				{
					Kind: WipeCoverKind,
					ID:   "wipe-cover",
					Props: map[string]json.RawMessage{
						"leaf":       raw(`"` + m10LeafPath + `"`),
						"reveal_ms":  raw(`400`),
						"hold_ms":    raw(`500`),
						"retract_ms": raw(`400`),
					},
				},
			},
		},
	}
}

func compileM10(t *testing.T) *RenderBundle {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": m10WipeCoverLayout("v1")},
		blueprints: map[string]*BlueprintGraph{},
		components: map[ComponentRef]*UserComponent{},
		manifest:   pureManifest(),
	}
	_, bundle, _, err := Compile(context.Background(), "scene-m10",
		PushEnvelope{CanvasVersion: "v1"}, f)
	if err != nil {
		t.Fatalf("compile M10 scene: %v", err)
	}
	return bundle
}

// oracleWipeCoverNode is the canonical wipe-cover RenderNode shape, replicated
// verbatim from Solar/src/overlay/wipe-cover.ts::buildWipeCoverNode (the parity
// oracle, ADR 003 §A5.3). It is the SINGLE source of truth for the keyframe
// geometry; Orion's lowering output (lowerWipeCover) must decode-equal this for
// the same timings. reveal=400 hold=500 retract=400, total=1300; magenta fill.
//
// This mirror is the in-repo half of the parity invariant (RC #64 (b)); the
// cross-repo byte-parity against the live TS builder is Pulsar#89 (Probe/Conduit).
func oracleWipeCoverNode(leaf, fill string, reveal, hold, retract int) map[string]any {
	total := float64(reveal + hold + retract)
	revealAt := float64(reveal) / total
	holdEndAt := float64(reveal+hold) / total
	return map[string]any{
		"kind": "frame",
		"id":   "wipe-cover",
		"props": map[string]any{
			"width":      "100%",
			"height":     "100%",
			"background": fill,
		},
		"keyframes": map[string]any{
			"key":         leaf,
			"duration_ms": float64(reveal + hold + retract),
			"easing":      "ease-in-out",
			"steps": []any{
				map[string]any{"at": float64(0), "opacity": float64(0)},
				map[string]any{"at": revealAt, "opacity": float64(1)},
				map[string]any{"at": holdEndAt, "opacity": float64(1)},
				map[string]any{"at": float64(1), "opacity": float64(0)},
			},
		},
	}
}

// TestLowerWipeCover_M10ServedBundle proves RC #64 (a): compiling the M10 scene
// yields a served RenderBundle whose tree contains the keyframed wipe-cover node
// — keyed on the declared scene_control leaf, magenta, with the 4-step
// reveal/hold/retract sequence. The node is the lowered FRAME (not the authoring
// `wipe-cover` kind), carries `keyframes`, and the authoring `wipe-cover` props
// (leaf/reveal_ms/…) are gone from the served props.
func TestLowerWipeCover_M10ServedBundle(t *testing.T) {
	bundle := compileM10(t)

	node := findNode(bundle.Root, "wipe-cover")
	if node == nil {
		t.Fatal("served bundle: wipe-cover node not found under root")
	}
	if node.Kind != "frame" {
		t.Fatalf("served wipe-cover node kind = %q, want lowered to %q", node.Kind, "frame")
	}
	if len(node.Keyframes) == 0 {
		t.Fatal("served wipe-cover node carries no keyframes block")
	}
	// Authoring props must be gone — the served node is the lowered frame.
	for _, k := range []string{"leaf", "reveal_ms", "hold_ms", "retract_ms"} {
		if _, leaked := node.Props[k]; leaked {
			t.Fatalf("authoring prop %q leaked into served frame props: %v", k, keysOf(node.Props))
		}
	}
	jsonEq(t, node, "width", "100%")
	jsonEq(t, node, "height", "100%")
	jsonEq(t, node, "background", "#C81E5A") // franc magenta cover (probe asserts MID=magenta)

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
	if kf.Key != m10LeafPath {
		t.Fatalf("keyframes.key = %q, want %q", kf.Key, m10LeafPath)
	}
	if kf.DurationMS != 1300 {
		t.Fatalf("keyframes.duration_ms = %d, want 1300 (400+500+400)", kf.DurationMS)
	}
	if kf.Easing != "ease-in-out" {
		t.Fatalf("keyframes.easing = %q, want ease-in-out", kf.Easing)
	}
	if len(kf.Steps) != 4 {
		t.Fatalf("keyframes.steps len = %d, want 4 (reveal/hold/retract)", len(kf.Steps))
	}
	// reveal 0→1, hold 1, retract 1→0 (first.at==0, last.at==1 — runtime
	// compileForFramer requires these endpoints).
	wantOpacity := []float64{0, 1, 1, 0}
	for i, s := range kf.Steps {
		if s.Opacity != wantOpacity[i] {
			t.Fatalf("step %d opacity = %v, want %v", i, s.Opacity, wantOpacity[i])
		}
	}
	if kf.Steps[0].At != 0 || kf.Steps[3].At != 1 {
		t.Fatalf("keyframe endpoints: first.at=%v last.at=%v, want 0 and 1", kf.Steps[0].At, kf.Steps[3].At)
	}
}

// TestLowerWipeCover_ParityWithBuildWipeCoverNode proves RC #64 (b): the node
// Orion emits is shape-identical (decoded) to Solar's buildWipeCoverNode for the
// same overlay timings, across several tuples including the edge
// reveal=hold=retract. Parity is the single-source-of-truth invariant (A5.3) —
// if Orion's times[]/key/duration_ms drift from the oracle, this fails loudly.
func TestLowerWipeCover_ParityWithBuildWipeCoverNode(t *testing.T) {
	cases := []struct{ reveal, hold, retract int }{
		{400, 500, 400}, // the fixture timings
		{200, 100, 300},
		{1, 1, 1},       // edge: reveal == hold == retract
		{1000, 1, 1000}, // wide reveal/retract, thin hold
		{333, 333, 333}, // repeating-decimal ratios — float64 parity check
	}
	for _, c := range cases {
		node := LayoutNode{
			Kind: WipeCoverKind,
			ID:   "wipe-cover",
			Props: map[string]json.RawMessage{
				"leaf":       json.RawMessage(`"` + m10LeafPath + `"`),
				"reveal_ms":  mustMarshal(c.reveal),
				"hold_ms":    mustMarshal(c.hold),
				"retract_ms": mustMarshal(c.retract),
			},
		}
		lowered, ok := lowerWipeCover(node)
		if !ok {
			t.Fatalf("%+v: lowerWipeCover rejected a well-formed element", c)
		}

		// Marshal Orion's lowered node the way the served bundle does, then
		// decode to a generic map for shape comparison against the oracle.
		gotRaw, err := json.Marshal(lowered)
		if err != nil {
			t.Fatalf("%+v: marshal lowered node: %v", c, err)
		}
		var got map[string]any
		if err := json.Unmarshal(gotRaw, &got); err != nil {
			t.Fatalf("%+v: decode lowered node: %v", c, err)
		}

		want := oracleWipeCoverNode(m10LeafPath, "#C81E5A", c.reveal, c.hold, c.retract)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v: lowered node diverges from buildWipeCoverNode oracle:\n got=%v\nwant=%v", c, got, want)
		}
	}
}

// TestLowerWipeCover_KeyframesRoundTrip proves RC #64 (d): the Keyframes field
// survives a json.Unmarshal of the served bundle (no silent drop), and is
// byte-stable across a marshal→unmarshal round-trip.
func TestLowerWipeCover_KeyframesRoundTrip(t *testing.T) {
	bundle := compileM10(t)

	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	// The served wire form MUST carry `keyframes` (the runtime reads it).
	var back RenderBundle
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	node := findNode(back.Root, "wipe-cover")
	if node == nil {
		t.Fatal("round-trip: wipe-cover node lost")
	}
	if len(node.Keyframes) == 0 {
		t.Fatal("round-trip: keyframes dropped on Unmarshal")
	}
	orig := findNode(bundle.Root, "wipe-cover")
	var a, b any
	if err := json.Unmarshal(orig.Keyframes, &a); err != nil {
		t.Fatalf("decode original keyframes: %v", err)
	}
	if err := json.Unmarshal(node.Keyframes, &b); err != nil {
		t.Fatalf("decode round-tripped keyframes: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("keyframes changed across round-trip:\n orig=%v\nback=%v", a, b)
	}
}

// TestLowerWipeCover_LSMLHashUnperturbed is the in-repo witness for
// SPIKE-LSML-HASH (ADR 003 §A5.5): authoring a wipe-cover element does NOT feed
// the C4 LSML content-hash, because EmitLSML reads the AUTHORING tree
// (bundle.AuthoringRoot), where the node is still the opaque `wipe-cover`
// authoring kind — the keyframes block lives ONLY on the lowered Root. So the
// LSML bundle carries no `keyframes`/`frame` for this node, the hash is the same
// whether or not Orion can later lower it, and adopt-on-verify
// (scenes_push.go:LSML_HASH_MISMATCH) is unperturbed: render-bundle-only.
func TestLowerWipeCover_LSMLHashUnperturbed(t *testing.T) {
	bundle := compileM10(t)

	// EmitLSML is the exact production C4 path (scenes_push.go:215).
	lsmlBundle, hashA, _, err := EmitLSML(
		"scene-m10", bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}

	// The authoring node in the LSML bundle is the wipe-cover element verbatim —
	// NOT lowered, NO keyframes block, NO `frame` kind for it.
	authoring := lsmlFindNode(t, lsmlBundle.Layout, "wipe-cover")
	if k, _ := authoring["kind"].(string); k != WipeCoverKind {
		t.Fatalf("LSML node kind = %q, want authoring %q (keyframes lowering must not reach the LSML tree)", k, WipeCoverKind)
	}
	if _, leaked := authoring["keyframes"]; leaked {
		t.Fatal("keyframes leaked into the LSML authoring tree — would perturb the C4 hash (A5.5 violation)")
	}
	// The authoring timings survive (opaque props), so Prism's matching
	// authoring node hashes identically — the adopt-on-verify byte-match holds.
	for _, k := range []string{"reveal_ms", "hold_ms", "retract_ms", "leaf"} {
		if _, ok := authoring[k]; !ok {
			t.Fatalf("authoring prop %q absent from LSML node — Prism's authoring tree would diverge", k)
		}
	}

	// Determinism: re-emitting the same authoring tree yields the same hash
	// (the property adopt-on-verify relies on).
	_, hashB, _, err := EmitLSML(
		"scene-m10", bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML (2nd): %v", err)
	}
	if hashA != hashB {
		t.Fatalf("LSML hash not deterministic across emits: %q vs %q", hashA, hashB)
	}
}

// TestLowerWipeCover_MalformedFallsThrough proves the inert-fallthrough stance:
// a wipe-cover element missing a required timing (or the leaf) is NOT lowered to
// a half-built keyframe — it stays a pass-through node so the runtime renders
// nothing, never a broken cover (mirrors Solar parseWipeCoverOverlay).
func TestLowerWipeCover_MalformedFallsThrough(t *testing.T) {
	cases := map[string]LayoutNode{
		"missing leaf": {
			Kind: WipeCoverKind,
			Props: map[string]json.RawMessage{
				"reveal_ms": json.RawMessage(`400`), "hold_ms": json.RawMessage(`500`), "retract_ms": json.RawMessage(`400`),
			},
		},
		"zero timing": {
			Kind: WipeCoverKind,
			Props: map[string]json.RawMessage{
				"leaf": json.RawMessage(`"x"`), "reveal_ms": json.RawMessage(`0`), "hold_ms": json.RawMessage(`500`), "retract_ms": json.RawMessage(`400`),
			},
		},
		"non-integer timing": {
			Kind: WipeCoverKind,
			Props: map[string]json.RawMessage{
				"leaf": json.RawMessage(`"x"`), "reveal_ms": json.RawMessage(`400.5`), "hold_ms": json.RawMessage(`500`), "retract_ms": json.RawMessage(`400`),
			},
		},
	}
	for name, node := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok := lowerWipeCover(node)
			if ok {
				t.Fatalf("malformed wipe-cover (%s) was lowered — want fall-through", name)
			}
			// Through the tree driver it stays the authoring kind (inert).
			out := lowerRenderTree(node, nil)
			if out.Kind != WipeCoverKind {
				t.Fatalf("malformed wipe-cover lowered to %q, want pass-through %q", out.Kind, WipeCoverKind)
			}
			if len(out.Keyframes) != 0 {
				t.Fatal("malformed wipe-cover emitted a keyframes block")
			}
		})
	}
}
