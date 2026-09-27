package compiler

import (
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
