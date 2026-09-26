package compiler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const staticTestAssetBase = "https://assets.example.test/scene-assets"

func TestStaticRenderBundleMatchesSerializedPath(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for name, raw := range map[string]string{
		"minimal":                         `{"layout":{"kind":"frame"}}`,
		"assets and editable transitions": `{"layout":{"kind":"frame","children":[{"kind":"image","id":"logo","src":"assets/` + hash + `.png","size":{"w":320,"h":180},"bindAnimate":{"transform.translate":"__editable.logo.translate"},"animate":{"transition":{"duration":0,"easing":"linear"}}},{"kind":"text","bind":{"value":"title"},"style":{"fontSize":28,"color":"#fff"}}]},"defaults":{"title":"A < B & C","__editable.logo.translate":[25,40],"logo":"assets/` + hash + `.png","large":9007199254740993},"profiles":["x-figma.authoring/1"],"assets":{"allowedHosts":[],"fonts":[]},"operator_inputs":[{"path":"title","type":"string"}],"external_adapters":[]}`,
		"compiled passthrough":            ` { "scene_version":"original", "root":{"kind":"image","props":{"src":"assets/` + hash + `.png"}},"defaults":{"logo":"assets/` + hash + `.png"}, "future_field":123 } `,
	} {
		t.Run(name, func(t *testing.T) {
			encoded, wantDefaults, err := CompileStaticLSML([]byte(raw), "scene", "version", staticTestAssetBase)
			if err != nil {
				t.Fatal(err)
			}
			var decoded RenderBundle
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			got, defaults, err := CompileStaticRenderBundle([]byte(raw), "scene", "version", staticTestAssetBase)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, _ := json.Marshal(decoded)
			gotJSON, err := json.Marshal(got)
			if err != nil || !bytes.Equal(wantJSON, gotJSON) || !reflect.DeepEqual(defaults, wantDefaults) {
				t.Fatalf("typed/decoded mismatch: %s != %s, err=%v", gotJSON, wantJSON, err)
			}
			if name == "compiled passthrough" && string(encoded) != raw {
				t.Fatal("persisted compiled bytes changed")
			}
		})
	}
}

func TestStaticRenderBundleDefaultsHaveIndependentOwnership(t *testing.T) {
	for _, tree := range []string{`"layout":{"kind":"frame"}`, `"root":{"kind":"frame"}`} {
		bundle, defaults, err := CompileStaticRenderBundle([]byte(`{`+tree+`,"defaults":{"x":123}}`), "scene", "version", staticTestAssetBase)
		if err != nil {
			t.Fatal(err)
		}
		defaults["x"][0] = '9'
		delete(defaults, "x")
		if string(bundle.Defaults["x"]) != "123" {
			t.Fatal("mutable seeds aliased immutable bundle")
		}
	}
}

func TestStaticRenderBundleErrorParity(t *testing.T) {
	for _, raw := range []string{`{`, `{}`, `{"layout":null}`, `{"layout":1}`, `{"layout":{}}`, `{"layout":{"kind":"image","src":"assets/` + strings.Repeat("b", 64) + `"},"assets":[]}`} {
		_, _, oldErr := CompileStaticLSML([]byte(raw), "scene", "version", staticTestAssetBase)
		_, _, err := CompileStaticRenderBundle([]byte(raw), "scene", "version", staticTestAssetBase)
		if err == nil || oldErr == nil || err.Error() != oldErr.Error() {
			t.Fatalf("%s: %v != %v", raw, err, oldErr)
		}
	}
	_, _, err := CompileStaticRenderBundle([]byte(`{"root":42}`), "scene", "version", staticTestAssetBase)
	if !errors.Is(err, ErrInvalidStaticRenderBundle) {
		t.Fatalf("invalid compiled root: %v", err)
	}
}

// Compare the actual old Preview sequence against the typed path, including
// unchanged image data carried in defaults. This excludes HTTP/paint time.
func BenchmarkStaticPreviewCompilation(b *testing.B) {
	for _, count := range []int{25, 250} {
		var children []map[string]any
		defaults := make(map[string]any)
		for i := 0; i < count; i++ {
			id := fmt.Sprintf("image-%d", i)
			children = append(children, map[string]any{"kind": "image", "id": id, "size": map[string]int{"w": 320, "h": 180}, "bind": map[string]string{"src": id}})
			defaults[id] = "data:image/png;base64," + strings.Repeat("ABCD", 1024)
		}
		raw, err := json.Marshal(map[string]any{"layout": map[string]any{"kind": "frame", "children": children}, "defaults": defaults})
		if err != nil {
			b.Fatal(err)
		}
		for _, typed := range []bool{false, true} {
			b.Run(fmt.Sprintf("images%d/typed%t", count, typed), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(raw)))
				for i := 0; i < b.N; i++ {
					if typed {
						if _, _, err := CompileStaticRenderBundle(raw, "scene", "version", staticTestAssetBase); err != nil {
							b.Fatal(err)
						}
					} else {
						encoded, _, err := CompileStaticLSML(raw, "scene", "version", staticTestAssetBase)
						if err != nil {
							b.Fatal(err)
						}
						var bundle RenderBundle
						if err := json.Unmarshal(encoded, &bundle); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}
