package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
)

const validateRenderBundleScope = "orion.validate.render-bundle"
const maxValidateRenderBundleBody = 1 << 20

type validateRenderBundleRequest struct {
	SceneID      string          `json:"scene_id"`
	SceneVersion string          `json:"scene_version"`
	LSMLBundle   json.RawMessage `json:"lsml_bundle"`
}

type validateRenderBundleResponse struct {
	Compiled           bool   `json:"compiled"`
	RenderBundle       string `json:"render_bundle,omitempty"`
	RenderBundleDigest string `json:"render_bundle_digest,omitempty"`
	Code               string `json:"code,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

// postValidateRenderBundle compiles an authoring LSML bundle without
// mutating the process host. ZabCanvas calls this during validation and
// persists the exact returned bytes for later scene-intent loads.
func postValidateRenderBundle(deps SceneIntentDeps) http.HandlerFunc {
	return requireServiceScopeOrOperator(validateRenderBundleScope, func(w http.ResponseWriter, r *http.Request) {
		raw, err := readBounded(r.Body, maxValidateRenderBundleBody)
		if errors.Is(err, errBodyTooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "BODY_TOO_LARGE"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_BODY"})
			return
		}
		var body validateRenderBundleRequest
		if json.Unmarshal(raw, &body) != nil || body.SceneID == "" || body.SceneVersion == "" || len(body.LSMLBundle) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_BODY"})
			return
		}
		if deps.StaticBundleCompiler == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "STATIC_COMPILER_UNAVAILABLE"})
			return
		}
		compiled, _, compileErr := deps.StaticBundleCompiler(body.LSMLBundle, body.SceneID, body.SceneVersion)
		if compileErr != nil {
			writeJSON(w, http.StatusOK, validateRenderBundleResponse{
				Compiled: false,
				Code:     "STATIC_BUNDLE_COMPILE_FAILED",
				Reason:   compileErr.Error(),
			})
			return
		}
		sum := sha256.Sum256(compiled)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		writeJSON(w, http.StatusOK, validateRenderBundleResponse{
			Compiled:           true,
			RenderBundle:       base64.StdEncoding.EncodeToString(compiled),
			RenderBundleDigest: digest,
		})
	})
}
