package compiler

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCompileStaticLSMLProducesSolarBundleAndDefaults(t *testing.T) {
	const assetHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	raw := []byte(`{
  "scene_id":"scene-1",
  "layout":{"kind":"frame","size":{"w":1920,"h":1080},"style":{"background":"#102030"},"children":[
    {"kind":"text","bind":{"value":"__lit.text.title"},"style":{"fontSize":56,"color":"#ffffff"}},
    {"kind":"image","bind":{"src":"__lit.image.logo"},"size":{"w":320,"h":180}}
  ]},
  "defaults":{"__lit.text.title":"Launch","__lit.image.logo":"assets/` + assetHash + `.png"},
  "assets":{"allowedHosts":[]},
  "profiles":["x-figma.authoring/1"]
}`)

	bundleBytes, defaults, err := CompileStaticLSML(raw, "scene-1", "sha256:scene", "https://zabgate.test/canvas/api/v1/scene-assets")
	if err != nil {
		t.Fatalf("CompileStaticLSML: %v", err)
	}
	var bundle RenderBundle
	if err := json.Unmarshal(bundleBytes, &bundle); err != nil {
		t.Fatalf("decode RenderBundle: %v", err)
	}
	if bundle.SceneVersion != "sha256:scene" || bundle.Root.Kind != "frame" {
		t.Fatalf("bundle identity/root = (%q, %q)", bundle.SceneVersion, bundle.Root.Kind)
	}
	var width float64
	if err := json.Unmarshal(bundle.Root.Props["width"], &width); err != nil || width != 1920 {
		t.Fatalf("frame width = %s, want 1920", bundle.Root.Props["width"])
	}
	if len(bundle.Root.Children) != 2 || bundle.Root.Children[0].Bindings["value"] != "__lit.text.title" {
		t.Fatalf("compiled children/binding = %#v", bundle.Root.Children)
	}
	var imageURL string
	if err := json.Unmarshal(defaults["__lit.image.logo"], &imageURL); err != nil {
		t.Fatalf("decode image default: %v", err)
	}
	wantURL := "https://zabgate.test/canvas/api/v1/scene-assets/" + assetHash + "/bytes"
	if imageURL != wantURL {
		t.Fatalf("image default = %q, want %q", imageURL, wantURL)
	}
	var assets map[string]any
	if err := json.Unmarshal(bundle.LSMLAssets, &assets); err != nil {
		t.Fatalf("decode assets: %v", err)
	}
	if assets["allowedHosts"].([]any)[0] != "zabgate.test" {
		t.Fatalf("allowedHosts = %#v", assets["allowedHosts"])
	}
}

func TestCompileStaticLSMLRejectsMissingRenderTree(t *testing.T) {
	if _, _, err := CompileStaticLSML([]byte(`{"scene_id":"scene-1"}`), "scene-1", "sha256:scene", "https://zabgate.test/canvas/api/v1/scene-assets"); err == nil {
		t.Fatal("expected malformed static bundle to be rejected")
	}
}

func TestCompileStaticLSMLKeepsValidChildrenAroundMalformedEntries(t *testing.T) {
	raw := []byte(`{"layout":{"kind":"frame","children":[{"kind":"text","id":"before"},17,{"kind":"shape","id":"after"}]}}`)
	encoded, _, err := CompileStaticLSML(raw, "scene", "sha256:scene", staticTestAssetBase)
	if err != nil {
		t.Fatalf("compile static LSML: %v", err)
	}
	var bundle RenderBundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatalf("decode RenderBundle: %v", err)
	}
	if len(bundle.Root.Children) != 2 || bundle.Root.Children[0].ID != "before" || bundle.Root.Children[1].ID != "after" {
		t.Fatalf("children = %#v; want valid siblings around ignored scalar", bundle.Root.Children)
	}
}

func TestScanStaticJSONFieldsMatchesEncodingJSON(t *testing.T) {
	const raw = ` {
  "kind":"frame",
  "kind":"text",
  "n\u0061me":"escaped key",
  "children":[{"value":"quotes: \" ; delimiters: ] }","meta":{"slash":"\\\\","close":"\u007d"}}],
  "enabled":true,
  "nil":null
 } `
	var want map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	got, ok := scanJSONObjectFields([]byte(raw))
	if !ok {
		t.Fatal("scanner rejected valid JSON object")
	}
	if len(got) != len(want) {
		t.Fatalf("scanner returned %d fields; encoding/json returned %d", len(got), len(want))
	}
	for key, expected := range want {
		if actual, exists := got[key]; !exists || !bytes.Equal(actual, expected) {
			t.Errorf("field %q = %q (exists %v), want %q", key, actual, exists, expected)
		}
	}
}

func FuzzScanStaticJSONObjectFieldsParity(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"kind":"frame","children":[{"value":"}\" ]"}]}`,
		`{"duplicate":1,"duplicate":2,"escaped\u006bey":true}`,
	} {
		f.Add(seed)
	}
	f.Add(string([]byte{'{', '"', 0xff, '"', ':', '1', '}'}))
	f.Fuzz(func(t *testing.T, input string) {
		var want map[string]json.RawMessage
		if err := json.Unmarshal([]byte(input), &want); err != nil || want == nil {
			return
		}
		got, ok := scanJSONObjectFields([]byte(input))
		if !ok {
			t.Fatalf("scanner rejected valid object %q", input)
		}
		if len(got) != len(want) {
			t.Fatalf("field count = %d, want %d", len(got), len(want))
		}
		for key, expected := range want {
			if actual, exists := got[key]; !exists || !bytes.Equal(actual, expected) {
				t.Fatalf("field %q = %q (exists %v), want %q", key, actual, exists, expected)
			}
		}
	})
}

func TestScanStaticJSONArrayElementsMatchesEncodingJSON(t *testing.T) {
	const raw = `[ {"kind":"text"}, 17, null, "quoted ] } and escaped quote: \\\"", {"kind":"frame","children":[]} ]`
	var want []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	var got []string
	ok, err := scanJSONArrayElements([]byte(raw), func(value []byte) error {
		got = append(got, string(value))
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("scanner result = (%v, %v), want (true, nil)", ok, err)
	}
	if len(got) != len(want) {
		t.Fatalf("scanner returned %d elements; encoding/json returned %d", len(got), len(want))
	}
	for index, expected := range want {
		if got[index] != string(expected) {
			t.Errorf("element %d = %q, want %q", index, got[index], expected)
		}
	}
	if ok, err := scanJSONArrayElements([]byte(`{"not":"an array"}`), func([]byte) error { return nil }); ok || err != nil {
		t.Fatalf("object input result = (%v, %v), want (false, nil)", ok, err)
	}
}

func FuzzScanStaticJSONArrayElementsParity(f *testing.F) {
	for _, seed := range []string{
		`[]`,
		`[1,null,{"kind":"frame"}]`,
		`["escaped quote: \\\" and bracket ]",[1,{"v":2}]]`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		data := []byte(input)
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			return
		}
		var want []json.RawMessage
		if err := json.Unmarshal(data, &want); err != nil {
			return
		}
		var got []string
		ok, err := scanJSONArrayElements(data, func(value []byte) error {
			got = append(got, string(value))
			return nil
		})
		if err != nil || !ok {
			t.Fatalf("scanner result = (%v, %v), want (true, nil)", ok, err)
		}
		if len(got) != len(want) {
			t.Fatalf("element count = %d, want %d", len(got), len(want))
		}
		for index, expected := range want {
			if got[index] != string(expected) {
				t.Fatalf("element %d = %q, want %q", index, got[index], expected)
			}
		}
	})
}

func TestCompileStaticLSMLFastNodeDecoderPreservesDuplicateAndEscapedKeys(t *testing.T) {
	raw := []byte(`{"layout":{"kind":"frame","children":[{"k\u0069nd":"shape","kind":"text","id":"before","id":"after","value":"closing } ] and quote: \""}]}}`)
	encoded, _, err := CompileStaticLSML(raw, "scene", "sha256:scene", staticTestAssetBase)
	if err != nil {
		t.Fatalf("compile static LSML: %v", err)
	}
	var bundle RenderBundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatalf("decode render bundle: %v", err)
	}
	if len(bundle.Root.Children) != 1 {
		t.Fatalf("children = %#v, want one", bundle.Root.Children)
	}
	child := bundle.Root.Children[0]
	if child.Kind != "text" || child.ID != "after" || string(child.Props["value"]) != `"closing } ] and quote: \""` {
		t.Fatalf("child = %#v, want last duplicate fields and escaped value", child)
	}
}

func TestCompileStaticRenderBundleOwnsInputLayoutBytes(t *testing.T) {
	raw := []byte(`{"layout":{"kind":"text","value":"original"}}`)
	bundle, _, err := CompileStaticRenderBundle(raw, "scene", "sha256:scene", staticTestAssetBase)
	if err != nil {
		t.Fatalf("compile static render bundle: %v", err)
	}
	index := bytes.Index(raw, []byte("original"))
	if index < 0 {
		t.Fatal("test input value missing")
	}
	raw[index] = 'm'
	if got := string(bundle.Root.Props["value"]); got != `"original"` {
		t.Fatalf("compiled value aliases caller input: got %q", got)
	}
}

func TestCompileStaticLSMLPreservesEditableBindAnimate(t *testing.T) {
	raw := []byte(`{
  "layout":{"kind":"shape","id":"panel","position":{"x":0,"y":0},"bindAnimate":{"transform.translate":"__editable.70616e656c.translate"},"animate":{"transition":{"duration":0,"easing":"linear"}}},
  "defaults":{"__editable.70616e656c.translate":[25,40]}
}`)
	bundleBytes, defaults, err := CompileStaticLSML(raw, "editable-1", "sha256:editable", "https://zabgate.test/canvas/api/v1/scene-assets")
	if err != nil {
		t.Fatalf("CompileStaticLSML: %v", err)
	}
	var bundle RenderBundle
	if err := json.Unmarshal(bundleBytes, &bundle); err != nil {
		t.Fatalf("decode RenderBundle: %v", err)
	}
	if got := bundle.Root.AnimateBindings["transform.translate"]; got != "__editable.70616e656c.translate" {
		t.Fatalf("animate binding = %q", got)
	}
	if string(bundle.Root.Transitions["x"]) != `{"kind":"tween","duration_ms":0,"ease":"linear"}` || string(bundle.Root.Transitions["y"]) != `{"kind":"tween","duration_ms":0,"ease":"linear"}` {
		t.Fatalf("translate transitions = %#v", bundle.Root.Transitions)
	}
	if string(defaults["__editable.70616e656c.translate"]) != `[25,40]` {
		t.Fatalf("translate default = %s", defaults["__editable.70616e656c.translate"])
	}
}

func TestRewriteJSONFastPathPreservesUnchangedFragment(t *testing.T) {
	raw := json.RawMessage(` { "label" : "Match intro", "large" : 9007199254740993 } `)
	for _, test := range []struct {
		name    string
		rewrite func(json.RawMessage, string) (json.RawMessage, bool, error)
	}{
		{name: "defensive", rewrite: rewriteJSON},
		{name: "parent validated", rewrite: rewriteJSONValidatedParent},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, touched, err := test.rewrite(raw, "https://zabgate.test/canvas/api/v1/scene-assets")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if touched {
				t.Fatal("rewrite marked a fragment without asset references as touched")
			}
			if string(got) != string(raw) {
				t.Fatalf("rewrite changed untouched raw JSON:\n got: %s\nwant: %s", got, raw)
			}
		})
	}
}

func TestRewriteJSONFastPathFallsBackForEscapedAssetReferences(t *testing.T) {
	const assetHash = "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"
	for name, raw := range map[string]json.RawMessage{
		"escaped slash":  []byte(`{"src":"assets\/` + assetHash + `.png"}`),
		"escaped prefix": []byte(`{"src":"\u0061ssets/` + assetHash + `.png"}`),
	} {
		t.Run(name, func(t *testing.T) {
			for _, rewrite := range []struct {
				name     string
				function func(json.RawMessage, string) (json.RawMessage, bool, error)
			}{
				{name: "defensive", function: rewriteJSON},
				{name: "parent validated", function: rewriteJSONValidatedParent},
			} {
				t.Run(rewrite.name, func(t *testing.T) {
					got, touched, err := rewrite.function(raw, "https://zabgate.test/canvas/api/v1/scene-assets")
					if err != nil {
						t.Fatalf("rewrite: %v", err)
					}
					if !touched {
						t.Fatal("rewrite did not recognize escaped asset reference")
					}
					var value map[string]string
					if err := json.Unmarshal(got, &value); err != nil {
						t.Fatalf("decode rewritten JSON: %v", err)
					}
					want := "https://zabgate.test/canvas/api/v1/scene-assets/" + strings.ToLower(assetHash) + "/bytes"
					if value["src"] != want {
						t.Fatalf("rewritten source = %q, want %q", value["src"], want)
					}
				})
			}
		})
	}
}

func TestRewriteJSONFastPathStillRejectsInvalidJSON(t *testing.T) {
	if _, _, err := rewriteJSON(json.RawMessage(`{"style":}`), "https://zabgate.test/canvas/api/v1/scene-assets"); err == nil {
		t.Fatal("rewriteJSON accepted invalid JSON")
	}
}
