package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	mux, wire := editableAPIFixture(t, config.ProfileEmbeddedLocal, func(deps *SceneIntentDeps) { deps.StaticBundleCompiler = nil; deps.StaticRenderBundleCompiler = nil })
	fontBytes := []byte{1, 2, 3}
	fontHash := sha256.Sum256(fontBytes)
	fontDigest := hex.EncodeToString(fontHash[:])
	fontPath := "assets/" + fontDigest + ".ttf"
	request := map[string]any{"scene_id": "draft", "scene_version": version, "render_mode": "source-only", "lsml_bundle": json.RawMessage(raw), "assets": map[string][]byte{fontPath: fontBytes}}
	opened := editableAPIRequest(t, mux, http.MethodPost, request, "operator")
	if opened.Code != http.StatusOK {
		t.Fatalf("open: %d %s", opened.Code, opened.Body.String())
	}
	patched := editableAPIRequest(t, mux, http.MethodPut, map[string]any{"scene_id": "draft", "base_edit_seq": 0, "edit_seq": 1, "patches": []map[string]any{{"path": "__editable.61.x", "value": 42}}}, "operator")
	if patched.Code != http.StatusAccepted {
		t.Fatalf("patch: %d %s", patched.Code, patched.Body.String())
	}
	native := wire.source
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Version   string `json:"scene_version"`
		Authoring struct {
			Format string            `json:"format"`
			Base   json.RawMessage   `json:"lsml_bundle"`
			Assets map[string][]byte `json:"assets"`
			Fonts  []string          `json:"fonts"`
		} `json:"x-solar-authoring"`
	}
	if json.Unmarshal(native, &decoded) != nil || decoded.Version != version || decoded.Authoring.Format != "solar.authoring/1" || string(decoded.Authoring.Base) != string(raw) {
		t.Fatal("native authoring must preserve the immutable base and asset bytes")
	}
	if len(decoded.Authoring.Fonts) != 1 || decoded.Authoring.Fonts[0] != fontDigest || len(decoded.Authoring.Assets) != 0 {
		t.Fatal("native scene must reference preloaded fonts without their bytes")
	}
	invalidFont := editablePreviewOpenRequest{LSMLBundle: raw, Assets: map[string][]byte{fontPath: {9}}}
	if _, err := editableNativeSource(invalidFont); err == nil {
		t.Fatal("mismatched font content address accepted")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/show/editable-preview/source?scene_id=draft", nil)
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", "operator")
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("HTTP source route still available: %d", res.Code)
	}
	doc["layout"] = map[string]any{"kind": "shape", "width": json.Number("201")}
	request["lsml_bundle"] = doc
	rejected := editableAPIRequest(t, mux, http.MethodPost, request, "operator")
	if rejected.Code != 422 {
		t.Fatalf("tampered address admitted: %d %s", rejected.Code, rejected.Body.String())
	}
}
