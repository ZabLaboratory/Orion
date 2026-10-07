package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Local authoring verifies the immutable LSML address without lowering its tree.
// The private Preview clone still owns sequencing/defaults; no Blue admission is created.
func editableSourceBundle(body editablePreviewOpenRequest) (*compiler.RenderBundle, map[string]json.RawMessage, error) {
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body.LSMLBundle))
	decoder.UseNumber()
	if decoder.Decode(&document) != nil || document == nil {
		return nil, nil, errors.New("invalid editable LSML document")
	}
	version := document["lsml"]
	_, layout := document["layout"].(map[string]any)
	if (version != "1.0" && version != "1.1" && version != "1.2") || !layout || document["scene_id"] != body.SceneID || document["scene_version"] != body.SceneVersion {
		return nil, nil, errors.New("editable LSML identity or layout mismatch")
	}
	document["scene_version"] = "sha256:" + strings.Repeat("0", 64)
	digest, err := canonical.Digest(document)
	if err != nil || digest != body.SceneVersion {
		return nil, nil, errors.New("editable LSML content address mismatch")
	}
	var source struct {
		Defaults map[string]json.RawMessage `json:"defaults"`
	}
	if err := json.Unmarshal(body.LSMLBundle, &source); err != nil {
		return nil, nil, err
	}
	return &compiler.RenderBundle{SceneVersion: body.SceneVersion, Defaults: source.Defaults, SourceLSML: rawBundleCopy(body.LSMLBundle)}, source.Defaults, nil
}

func getEditablePreviewSource(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if !deps.Config.Profile.IsEmbeddedLocal() || deps.Preview == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		raw, assets, ok := deps.Preview.EditableSource(r.URL.Query().Get("scene_id"), r.URL.Query().Get("v"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "EDITABLE_SOURCE_NOT_FOUND"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Scene-Version", r.URL.Query().Get("v"))
		writeJSON(w, http.StatusOK, map[string]any{"lsml_bundle": json.RawMessage(raw), "assets": assets})
	})
}
