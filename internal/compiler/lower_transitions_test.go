package compiler

import (
	"encoding/json"
	"strings"
	"testing"
)

// Oracle reference for the EXACT M10 directive (from {opacity:0,scale:0.85},
// target {opacity:1, scale:1}, duration 1200, easing "ease-out"), produced by
// EXECUTING @lumencast/compiler@0.3.0 compileBundle on 2026-06-09
// (lumencast-js/packages/compiler/dist/compile.js):
//
//	transitions: {"opacity":{"kind":"tween","duration_ms":1200,"ease":"cubic-out"},
//	              "scale":{"kind":"tween","duration_ms":1200,"ease":"cubic-out"}}
//
// NOTE: the runtime contract field is `ease`, NOT `easing` (TweenTransition,
// runtime/src/animate/transitions.ts:16-20).
const m10PerPropTransitionsTS = `{"opacity":{"kind":"tween","duration_ms":1200,"ease":"cubic-out"},"scale":{"kind":"tween","duration_ms":1200,"ease":"cubic-out"}}`

// TestLowerTransitions_M10ParityTS proves the runtime-contract fix on the
// exact M10 directive: the lowered render-bundle node carries per-prop
// `transitions` BYTE-IDENTICAL to the TS compiler's output for the same
// directive (Go's sorted map keys coincide with the oracle's insertion order
// here: opacity → scale).
func TestLowerTransitions_M10ParityTS(t *testing.T) {
	bundle := compileM10Animated(t)

	node := findNode(bundle.Root, "zab-logo")
	if node == nil {
		t.Fatal("served bundle: zab-logo node not found")
	}
	got, err := json.Marshal(node.Transitions)
	if err != nil {
		t.Fatalf("marshal transitions: %v", err)
	}
	if string(got) != m10PerPropTransitionsTS {
		t.Fatalf("transitions = %s,\nwant TS-parity bytes %s", got, m10PerPropTransitionsTS)
	}
	// And the mount-play state still rides alongside (fix #66 untouched).
	if string(node.AnimateInitial) != `{"opacity":0,"scale":0.85}` {
		t.Fatalf("animate_initial = %s, want {\"opacity\":0,\"scale\":0.85}", node.AnimateInitial)
	}
}

// TestLowerTransitions_OracleShapes pins compileAnimate + the prop fan-out
// (compile.ts:199-217, 323-361) case by case. Every `want` value below is the
// EXECUTED oracle's output (compileBundle run 2026-06-09), not a guess:
// duration default 200, easing map linear/cubic-in/cubic-out/cubic-in-out,
// unknown easing → `ease` omitted, spring passthrough of stiffness/damping,
// translate fanning out to BOTH x and y.
func TestLowerTransitions_OracleShapes(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	cases := map[string]struct {
		in   map[string]json.RawMessage
		want string // marshalled per-prop map; "" → nil (field omitted)
	}{
		"tween no easing (oracle: no-easing)": {
			map[string]json.RawMessage{"opacity": raw(`1`), "transition": raw(`{"duration":800}`)},
			`{"opacity":{"kind":"tween","duration_ms":800}}`,
		},
		"tween no duration → 200 (oracle: no-duration)": {
			map[string]json.RawMessage{"opacity": raw(`1`), "transition": raw(`{"easing":"linear"}`)},
			`{"opacity":{"kind":"tween","duration_ms":200,"ease":"linear"}}`,
		},
		"ease-in → cubic-in": {
			map[string]json.RawMessage{"opacity": raw(`1`), "transition": raw(`{"duration":100,"easing":"ease-in"}`)},
			`{"opacity":{"kind":"tween","duration_ms":100,"ease":"cubic-in"}}`,
		},
		"ease-in-out → cubic-in-out": {
			map[string]json.RawMessage{"opacity": raw(`1`), "transition": raw(`{"duration":100,"easing":"ease-in-out"}`)},
			`{"opacity":{"kind":"tween","duration_ms":100,"ease":"cubic-in-out"}}`,
		},
		"unknown easing → ease omitted (mapEase default)": {
			map[string]json.RawMessage{"opacity": raw(`1`), "transition": raw(`{"duration":100,"easing":"bounce"}`)},
			`{"opacity":{"kind":"tween","duration_ms":100}}`,
		},
		"spring with params (oracle: spring)": {
			map[string]json.RawMessage{"opacity": raw(`1`), "transition": raw(`{"easing":"spring","stiffness":120,"damping":14}`)},
			`{"opacity":{"kind":"spring","stiffness":120,"damping":14}}`,
		},
		"spring bare": {
			map[string]json.RawMessage{"opacity": raw(`1`), "transition": raw(`{"easing":"spring"}`)},
			`{"opacity":{"kind":"spring"}}`,
		},
		"rotate + translate fan-out (oracle: translate+rotate)": {
			map[string]json.RawMessage{"transform": raw(`{"rotate":90,"translate":[10,-20]}`), "transition": raw(`{"duration":300,"easing":"ease-in-out"}`)},
			`{"rotate":{"kind":"tween","duration_ms":300,"ease":"cubic-in-out"},"x":{"kind":"tween","duration_ms":300,"ease":"cubic-in-out"},"y":{"kind":"tween","duration_ms":300,"ease":"cubic-in-out"}}`,
		},
		"from only, no transition → omitted (oracle: from-only-no-transition = undefined)": {
			map[string]json.RawMessage{"from": raw(`{"opacity":0}`), "opacity": raw(`1`)},
			``,
		},
		"transition present, no animated prop → omitted (Object.keys guard)": {
			map[string]json.RawMessage{"transition": raw(`{"duration":300}`)},
			``,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := lowerTransitions(c.in)
			if c.want == "" {
				if got != nil {
					b, _ := json.Marshal(got)
					t.Fatalf("want nil (transitions omitted), got %s", b)
				}
				return
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != c.want {
				t.Fatalf("lowerTransitions = %s,\nwant %s", b, c.want)
			}
		})
	}
}

// TestLowerTransitions_RetroCompat proves the non-envelope paths are
// non-destructive: an ALREADY per-prop map (conforming producer) and a map
// with no envelope-signature key pass through byte-untouched; absent
// transitions stay absent.
func TestLowerTransitions_RetroCompat(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }

	t.Run("per-prop passthrough", func(t *testing.T) {
		in := map[string]json.RawMessage{
			"opacity": raw(`{"kind":"tween","duration_ms":1200,"ease":"cubic-out"}`),
			"scale":   raw(`{"kind":"spring","stiffness":120}`),
		}
		got := lowerTransitions(in)
		if len(got) != len(in) {
			t.Fatalf("per-prop map altered: got %v", keysOf(got))
		}
		for k, v := range in {
			if string(got[k]) != string(v) {
				t.Fatalf("per-prop[%q] = %s, want untouched %s", k, got[k], v)
			}
		}
	})

	t.Run("unknown shape passthrough", func(t *testing.T) {
		// No transition/from/transform signature, not per-prop — do not destroy.
		in := map[string]json.RawMessage{"opacity": raw(`1`)}
		got := lowerTransitions(in)
		if len(got) != 1 || string(got["opacity"]) != `1` {
			t.Fatalf("unknown shape altered: %v", got)
		}
	})

	t.Run("no transitions", func(t *testing.T) {
		lowered := lowerRenderTree(LayoutNode{Kind: "image", ID: "logo"}, nil)
		if lowered.Transitions != nil {
			t.Fatalf("no-transitions node grew transitions: %v", lowered.Transitions)
		}
	})

	t.Run("from-only envelope keeps mount-play, drops transitions", func(t *testing.T) {
		lowered := lowerRenderTree(LayoutNode{
			Kind: "image", ID: "logo",
			Transitions: map[string]json.RawMessage{
				"from":    raw(`{"opacity":0}`),
				"opacity": raw(`1`),
			},
		}, nil)
		if lowered.Transitions != nil {
			t.Fatalf("from-only envelope should omit transitions (TS parity), got %v", keysOf(lowered.Transitions))
		}
		if string(lowered.AnimateInitial) != `{"opacity":0}` {
			t.Fatalf("animate_initial = %s, want {\"opacity\":0}", lowered.AnimateInitial)
		}
	})
}

// TestLowerTransitions_RoundTrip proves the per-prop map survives the
// served-wire round-trip byte-identically (no Unmarshal drop).
func TestLowerTransitions_RoundTrip(t *testing.T) {
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
	got, err := json.Marshal(node.Transitions)
	if err != nil {
		t.Fatalf("marshal round-tripped transitions: %v", err)
	}
	if string(got) != m10PerPropTransitionsTS {
		t.Fatalf("transitions changed across round-trip: %s", got)
	}
}

// TestLowerTransitions_LSMLHashUnperturbed is the SPIKE-LSML-HASH witness for
// this lowering (same stance as Keyframes / AnimateInitial): the per-prop map
// lives ONLY on the lowered Root; AuthoringRoot keeps the RAW envelope, so
// EmitLSML re-emits the authored `animate` map (transition + from verbatim)
// and the C4 adopt-on-verify hash is deterministic and unperturbed.
func TestLowerTransitions_LSMLHashUnperturbed(t *testing.T) {
	bundle := compileM10Animated(t)

	// The lowered Root carries per-prop (the fix)…
	if n := findNode(bundle.Root, "zab-logo"); n == nil || n.Transitions["opacity"] == nil ||
		!strings.Contains(string(n.Transitions["opacity"]), `"kind"`) {
		t.Fatal("lowered Root transitions are not per-prop — fix not applied")
	}
	// …and the authoring tree still holds the raw envelope.
	an := findNode(bundle.AuthoringRoot, "zab-logo")
	if an == nil {
		t.Fatal("AuthoringRoot: zab-logo node not found")
	}
	want := m10AnimateTransitions()
	if len(an.Transitions) != len(want) {
		t.Fatalf("AuthoringRoot transitions keys changed: %v", keysOf(an.Transitions))
	}
	for k, v := range want {
		if got, ok := an.Transitions[k]; !ok || string(got) != string(v) {
			t.Fatalf("AuthoringRoot transitions[%q] = %s, want raw envelope %s", k, got, v)
		}
	}

	// EmitLSML is the exact production C4 path (scenes_push.go).
	lsmlBundle, hashA, _, err := EmitLSML(
		"scene-m10-animated", bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}
	logo := lsmlFindNode(t, lsmlBundle.Layout, "zab-logo")
	animate, _ := logo["animate"].(map[string]any)
	if animate == nil {
		t.Fatalf("LSML zab-logo node lost its animate map: %v", keysOfAny(logo))
	}
	for _, k := range []string{"transition", "from", "opacity", "transform"} {
		if _, ok := animate[k]; !ok {
			t.Fatalf("authored animate.%s absent from LSML node — Prism's authoring tree would diverge: %v", k, keysOfAny(animate))
		}
	}
	if strings.Contains(string(lsmlBundle.Layout), `"duration_ms"`) {
		t.Fatal("per-prop transitions leaked into the LSML layout — C4 hash perturbed")
	}

	// Determinism: re-emitting yields the same hash (adopt-on-verify).
	_, hashB, _, err := EmitLSML(
		"scene-m10-animated", bundle.AuthoringRoot, bundle.OperatorInputs, bundle.ExternalAdapters, nil, nil,
	)
	if err != nil {
		t.Fatalf("EmitLSML (2nd): %v", err)
	}
	if hashA != hashB {
		t.Fatalf("LSML hash not deterministic: %q vs %q", hashA, hashB)
	}
}
