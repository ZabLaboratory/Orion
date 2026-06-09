package compiler

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// m10AnimateTransitions is the EXACT M10 `animate` directive as it arrives in
// LayoutNode.Transitions (Canvas folds the whole `animate` map into
// `transitions` on push — verified on the live served bundle, Pulsar runbook
// m10-animate-initial-contract-hole): from {opacity:0, scale:0.85} → target
// {opacity:1, scale:1}, 550 ms ease-out.
func m10AnimateTransitions() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"from":       json.RawMessage(`{"opacity":0,"transform":{"scale":0.85}}`),
		"opacity":    json.RawMessage(`1`),
		"transform":  json.RawMessage(`{"scale":1}`),
		"transition": json.RawMessage(`{"duration":550,"easing":"ease-out"}`),
	}
}

// m10AnimatedLayout authors the zab-transition logo node (Pulsar #97 fixture
// shape) as an Orion CanvasLayout: an image carrying the M10 animate
// directive in its transitions.
func m10AnimatedLayout(version string) *CanvasLayout {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	return &CanvasLayout{
		Version: version,
		Root: LayoutNode{
			Kind: "frame",
			ID:   "root",
			Props: map[string]json.RawMessage{
				"size":       raw(`{"w":1920,"h":1080}`),
				"background": raw(`"#FFFFFF"`),
			},
			Children: []LayoutNode{
				{
					Kind: "image",
					ID:   "zab-logo",
					Props: map[string]json.RawMessage{
						"alt": raw(`"Zablab logo"`),
						"fit": raw(`"contain"`),
						"src": raw(`"data:image/jpeg;base64,xxx"`),
					},
					Transitions: m10AnimateTransitions(),
				},
			},
		},
	}
}

func compileM10Animated(t *testing.T) *RenderBundle {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": m10AnimatedLayout("v1")},
		blueprints: map[string]*BlueprintGraph{},
		components: map[ComponentRef]*UserComponent{},
		manifest:   pureManifest(),
	}
	_, bundle, _, err := Compile(context.Background(), "scene-m10-animated",
		PushEnvelope{CanvasVersion: "v1"}, f)
	if err != nil {
		t.Fatalf("compile M10 animated scene: %v", err)
	}
	return bundle
}

// TestLowerAnimateInitial_M10Parity proves the contract fix on the exact M10
// directive: the lowered render-bundle node carries the flat
// `animate_initial` BYTE-IDENTICAL to what @lumencast/compiler@0.3.0's
// lowerAnimateState + JSON.stringify emit for the same from-state
// ({"opacity":0,"scale":0.85} — insertion order opacity→scale, ES6 number
// formatting), and `transitions` is left exactly as ingested (the runtime
// reads its timing).
func TestLowerAnimateInitial_M10Parity(t *testing.T) {
	bundle := compileM10Animated(t)

	node := findNode(bundle.Root, "zab-logo")
	if node == nil {
		t.Fatal("served bundle: zab-logo node not found")
	}
	const wantTS = `{"opacity":0,"scale":0.85}` // lowerAnimateState oracle output
	if got := string(node.AnimateInitial); got != wantTS {
		t.Fatalf("animate_initial = %s, want TS-parity bytes %s", got, wantTS)
	}
	// transitions untouched: same keys, same bytes, `from` still present.
	want := m10AnimateTransitions()
	if len(node.Transitions) != len(want) {
		t.Fatalf("transitions keys changed: got %v", keysOf(node.Transitions))
	}
	for k, v := range want {
		if got, ok := node.Transitions[k]; !ok || string(got) != string(v) {
			t.Fatalf("transitions[%q] = %s, want %s (must stay as ingested)", k, got, v)
		}
	}
	// The wire field name is the runtime's reader key.
	rawNode, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal node: %v", err)
	}
	if !strings.Contains(string(rawNode), `"animate_initial":{"opacity":0,"scale":0.85}`) {
		t.Fatalf("wire node missing flat animate_initial field: %s", rawNode)
	}
}

// TestLowerAnimateInitial_MappingParity pins the full lowerAnimateState
// mapping (compile.ts:240-255) key by key: opacity→opacity, scale scalar and
// [sx,sy]→sx, rotate→rotate, translate→x/y, emitted in the oracle's insertion
// order; non-number opacity/rotate skipped; zero keys → field omitted.
func TestLowerAnimateInitial_MappingParity(t *testing.T) {
	cases := map[string]struct {
		from string // "" → no from entry at all
		want string // "" → nil (field omitted)
	}{
		"m10 directive":     {`{"opacity":0,"transform":{"scale":0.85}}`, `{"opacity":0,"scale":0.85}`},
		"opacity only":      {`{"opacity":0.5}`, `{"opacity":0.5}`},
		"scale scalar":      {`{"transform":{"scale":2}}`, `{"scale":2}`},
		"scale pair → sx":   {`{"transform":{"scale":[0.5,0.9]}}`, `{"scale":0.5}`},
		"rotate":            {`{"transform":{"rotate":45}}`, `{"rotate":45}`},
		"translate → x/y":   {`{"transform":{"translate":[10,-20]}}`, `{"x":10,"y":-20}`},
		"all keys, ordered": {`{"opacity":0.5,"transform":{"scale":[2,3],"rotate":45,"translate":[10,-20]}}`, `{"opacity":0.5,"scale":2,"rotate":45,"x":10,"y":-20}`},
		"empty from":        {`{}`, ``},
		"opacity non-number skipped (typeof guard)": {`{"opacity":"0"}`, ``},
		"malformed from": {`"not-a-state"`, ``},
		"no from":        {``, ``},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tx := map[string]json.RawMessage{
				"transition": json.RawMessage(`{"duration":550,"easing":"ease-out"}`),
			}
			if c.from != "" {
				tx["from"] = json.RawMessage(c.from)
			}
			got := lowerAnimateInitial(tx)
			if c.want == "" {
				if got != nil {
					t.Fatalf("want nil (field omitted), got %s", got)
				}
				return
			}
			if string(got) != c.want {
				t.Fatalf("lowerAnimateInitial = %s, want %s", got, c.want)
			}
		})
	}
}

// TestLowerAnimateInitial_RetroCompat proves the no-`from` path is untouched:
// a node animated WITHOUT a from-state (or not animated at all) serves no
// `animate_initial` key on the wire — the runtime's prior no-mount-play
// behaviour holds (the TS compiler's same guard, compile.ts:224-229).
func TestLowerAnimateInitial_RetroCompat(t *testing.T) {
	node := LayoutNode{
		Kind: "image",
		ID:   "logo",
		Transitions: map[string]json.RawMessage{
			"opacity":    json.RawMessage(`1`),
			"transition": json.RawMessage(`{"duration":550,"easing":"ease-out"}`),
		},
	}
	lowered := lowerRenderTree(node)
	if lowered.AnimateInitial != nil {
		t.Fatalf("no-from node got animate_initial = %s, want absent", lowered.AnimateInitial)
	}
	raw, err := json.Marshal(lowered)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "animate_initial") {
		t.Fatalf("animate_initial leaked onto the wire for a no-from node: %s", raw)
	}
}

// TestLowerAnimateInitial_RoundTrip proves the field survives the served-wire
// round-trip byte-identically (no Unmarshal drop — the additive-field
// invariant shared with Keyframes).
func TestLowerAnimateInitial_RoundTrip(t *testing.T) {
	bundle := compileM10Animated(t)

	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	var back RenderBundle
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	node := findNode(back.Root, "zab-logo")
	if node == nil {
		t.Fatal("round-trip: zab-logo node lost")
	}
	orig := findNode(bundle.Root, "zab-logo")
	if string(node.AnimateInitial) != string(orig.AnimateInitial) {
		t.Fatalf("animate_initial changed across round-trip: orig=%s back=%s",
			orig.AnimateInitial, node.AnimateInitial)
	}
	if len(node.AnimateInitial) == 0 {
		t.Fatal("round-trip: animate_initial dropped on Unmarshal")
	}
	var a, b any
	if err := json.Unmarshal(orig.AnimateInitial, &a); err != nil {
		t.Fatalf("decode original: %v", err)
	}
	if err := json.Unmarshal(node.AnimateInitial, &b); err != nil {
		t.Fatalf("decode round-tripped: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("animate_initial value changed: orig=%v back=%v", a, b)
	}
}

// TestLowerAnimateInitial_LSMLHashUnperturbed is the SPIKE-LSML-HASH witness
// for this field (same stance as Keyframes, A5.5): `animate_initial` lives
// ONLY on the lowered Root. EmitLSML reads the AUTHORING tree, where the
// from-state is still the opaque `transitions.from` entry — so the emitted
// LSML bundle carries no `animate_initial`, the authored `from` survives
// verbatim (Prism's matching authoring tree hashes identically), and the C4
// adopt-on-verify hash is deterministic and unperturbed.
func TestLowerAnimateInitial_LSMLHashUnperturbed(t *testing.T) {
	bundle := compileM10Animated(t)

	// The lowered Root DOES carry the field (the fix)…
	if n := findNode(bundle.Root, "zab-logo"); n == nil || len(n.AnimateInitial) == 0 {
		t.Fatal("lowered Root lacks animate_initial — fix not applied")
	}
	// …and the authoring tree does NOT.
	if n := findNode(bundle.AuthoringRoot, "zab-logo"); n == nil || n.AnimateInitial != nil {
		t.Fatal("AuthoringRoot carries animate_initial — would perturb the C4 hash")
	}

	// EmitLSML is the exact production C4 path (scenes_push.go:215).
	lsmlBundle, hashA, _, err := EmitLSML(
		"scene-m10-animated", bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}
	if strings.Contains(string(lsmlBundle.Layout), "animate_initial") {
		t.Fatal("animate_initial leaked into the LSML layout — C4 hash perturbed")
	}
	// The authored from-state survives in the LSML animate map (Prism parity).
	logo := lsmlFindNode(t, lsmlBundle.Layout, "zab-logo")
	animate, _ := logo["animate"].(map[string]any)
	if animate == nil {
		t.Fatalf("LSML zab-logo node lost its animate map: %v", keysOfAny(logo))
	}
	if _, ok := animate["from"]; !ok {
		t.Fatalf("authored animate.from absent from LSML node — Prism's authoring tree would diverge: %v", keysOfAny(animate))
	}

	// Determinism: re-emitting yields the same hash (adopt-on-verify relies on it).
	_, hashB, _, err := EmitLSML(
		"scene-m10-animated", bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML (2nd): %v", err)
	}
	if hashA != hashB {
		t.Fatalf("LSML hash not deterministic: %q vs %q", hashA, hashB)
	}
}
