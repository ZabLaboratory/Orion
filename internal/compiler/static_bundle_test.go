package compiler

import (
	"encoding/json"
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
