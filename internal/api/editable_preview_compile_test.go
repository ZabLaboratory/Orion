package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
)

func TestEditablePreviewTypedCompilerContracts(t *testing.T) {
	for _, typed := range []bool{false, true} {
		name := "bytes"
		if typed {
			name = "typed"
		}
		t.Run(name, func(t *testing.T) {
			mux, wire := editableAPIFixture(t, config.ProfileEmbeddedLocal, func(deps *SceneIntentDeps) {
				deps.StaticBundleCompiler = func(raw []byte, id, version string) ([]byte, map[string]json.RawMessage, error) {
					if typed {
						t.Fatal("typed Preview called the byte compiler")
					}
					return compiler.CompileStaticLSML(raw, id, version, "https://assets.test")
				}
				if typed {
					deps.StaticRenderBundleCompiler = func(raw []byte, id, version string) (*compiler.RenderBundle, map[string]json.RawMessage, error) {
						return compiler.CompileStaticRenderBundle(raw, id, version, "https://assets.test")
					}
				}
			})
			for _, tc := range []struct {
				raw    string
				status int
				code   string
			}{
				{`{"layout":{"kind":"shape"},"defaults":{"__editable.61.x":10}}`, http.StatusOK, `"paths":1`},
				{`{"layout":null}`, http.StatusUnprocessableEntity, "STATIC_BUNDLE_COMPILE_FAILED"},
				{`{"root":42}`, http.StatusInternalServerError, "STATIC_BUNDLE_INVALID"},
			} {
				result := editableAPIRequest(t, mux, http.MethodPost, map[string]any{"scene_id": "typed-scene", "scene_version": "version", "edit_seq": 4, "lsml_bundle": json.RawMessage(tc.raw)}, "operator")
				if result.Code != tc.status || !strings.Contains(result.Body.String(), tc.code) {
					t.Fatalf("open = %d %s", result.Code, result.Body.String())
				}
			}
			if wire.active != "typed-scene" {
				t.Fatalf("active=%s", wire.active)
			}
			for i, status := range []int{http.StatusAccepted, http.StatusConflict} {
				result := editableAPIRequest(t, mux, http.MethodPut, map[string]any{"scene_id": "typed-scene", "base_edit_seq": 4, "edit_seq": 5, "patches": []map[string]any{{"path": "__editable.61.x", "value": 25 + i}}}, "operator")
				if result.Code != status {
					t.Fatalf("patch = %d %s", result.Code, result.Body.String())
				}
			}
		})
	}
}

func TestEditablePreviewTypedOnlyAndNilBundle(t *testing.T) {
	for _, nilBundle := range []bool{false, true} {
		mux, _ := editableAPIFixture(t, config.ProfileEmbeddedLocal, func(deps *SceneIntentDeps) {
			deps.StaticBundleCompiler = nil
			deps.StaticRenderBundleCompiler = func([]byte, string, string) (*compiler.RenderBundle, map[string]json.RawMessage, error) {
				if nilBundle {
					return nil, nil, nil
				}
				return &compiler.RenderBundle{}, nil, nil
			}
		})
		result := editableAPIRequest(t, mux, http.MethodPost, map[string]any{"scene_id": "scene", "scene_version": "version", "lsml_bundle": map[string]any{}}, "operator")
		want := http.StatusOK
		if nilBundle {
			want = http.StatusInternalServerError
		}
		if result.Code != want {
			t.Fatalf("open = %d %s", result.Code, result.Body.String())
		}
	}
}
