package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/config"
)

func TestEditableSourceOnlyAddressAndImmutableBase(t *testing.T) {
	doc := map[string]any{"lsml": "1.2", "scene_id": "draft", "scene_version": "sha256:" + strings.Repeat("0", 64), "layout": map[string]any{"kind": "shape", "width": json.Number("200.5")}, "defaults": map[string]any{"__editable.61.x": json.Number("10")}}
	version, err := canonical.Digest(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc["scene_version"] = version
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	mux, _ := editableAPIFixture(t, config.ProfileEmbeddedLocal, func(deps *SceneIntentDeps) { deps.StaticBundleCompiler = nil; deps.StaticRenderBundleCompiler = nil })
	request := map[string]any{"scene_id": "draft", "scene_version": version, "render_mode": "source-only", "lsml_bundle": json.RawMessage(raw)}
	opened := editableAPIRequest(t, mux, http.MethodPost, request, "operator")
	if opened.Code != http.StatusOK {
		t.Fatalf("open: %d %s", opened.Code, opened.Body.String())
	}
	patched := editableAPIRequest(t, mux, http.MethodPut, map[string]any{"scene_id": "draft", "base_edit_seq": 0, "edit_seq": 1, "patches": []map[string]any{{"path": "__editable.61.x", "value": 42}}}, "operator")
	if patched.Code != http.StatusAccepted {
		t.Fatalf("patch: %d %s", patched.Code, patched.Body.String())
	}
	for _, tc := range []struct {
		version, role string
		status        int
	}{{version, "operator", 200}, {"wrong", "operator", 404}, {version, "", 403}, {version, "viewer", 403}} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/show/editable-preview/source?scene_id=draft&v="+url.QueryEscape(tc.version), nil)
		if tc.role != "" {
			req.Header.Set("X-Authenticated-User", "operator-1")
			req.Header.Set("X-Authenticated-Role", tc.role)
		}
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Fatalf("source %s: %d %s", tc.role, res.Code, res.Body.String())
		}
		if res.Code == 200 && (res.Header().Get("X-Scene-Version") != version) {
			t.Fatal("source version mismatch")
		}
		if res.Code == 200 {
			var envelope struct {
				LSML json.RawMessage `json:"lsml_bundle"`
			}
			if json.Unmarshal(res.Body.Bytes(), &envelope) != nil || string(envelope.LSML) != string(raw) {
				t.Fatal("source must preserve base bytes after hot patch")
			}
		}
	}
	doc["layout"] = map[string]any{"kind": "shape", "width": json.Number("201")}
	request["lsml_bundle"] = doc
	rejected := editableAPIRequest(t, mux, http.MethodPost, request, "operator")
	if rejected.Code != 422 {
		t.Fatalf("tampered address admitted: %d %s", rejected.Code, rejected.Body.String())
	}
}
