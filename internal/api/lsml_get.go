package api

import "net/http"

// getLSMLBundle serves the content-addressed LSML 1.1 bundle bytes at
// GET /api/v1/scenes/{id}/lsml-bundle?v={scene_version} (ADR 007 §C.2).
//
// This is a NEW, additive endpoint — it does NOT touch the bespoke
// render-bundle serve (getRenderBundle), which keeps returning the
// legacy RenderBundle byte-for-byte. Solar/@lumencast/runtime fetches
// the render bundle in LSML form here; Solar-old keeps using
// render-bundle. Both can coexist during the dual-wire parallel-run.
//
// Contract:
//   - bespoke mode (default): the endpoint is inert — 404 LSML_DISABLED.
//     No LSML is persisted in this mode, so there is nothing to serve;
//     a deploy without ORION_LSDP_MODE=dual|lsdp is a no-op.
//   - dual|lsdp mode: ?v= is REQUIRED (the artifact is immutable and
//     content-addressed). The bytes are served opaque with the same
//     immutable long-TTL cache + ETag contract as render-bundle. An
//     unknown ?v= → 404. A missing ?v= → 400.
//
// The Go side never walks the layout tree — the bundle is opaque bytes
// (lsml.Bundle.Layout is json.RawMessage); only the TS
// @lumencast/compiler interprets primitives (ADR 007 §5 R3).
func getLSMLBundle(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.Config.LSDPMode.PersistsLSML() {
			// Inert by default: nothing is persisted in bespoke mode.
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "LSML_DISABLED"})
			return
		}

		sceneID, ok := parseUUID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene id"})
			return
		}

		v := r.URL.Query().Get("v")
		if v == "" {
			// LSML is a content-addressed immutable artifact; pin it.
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "VERSION_REQUIRED"})
			return
		}

		raw, err := deps.Store.GetLSMLBundleByHash(r.Context(), sceneID, v)
		if err != nil {
			status, code := codeFromError(err)
			if status == http.StatusNotFound {
				code = "LSML_BUNDLE_NOT_FOUND"
			}
			writeJSON(w, status, map[string]string{"code": code})
			return
		}

		// Same immutable-cache contract as the bespoke artefact serve:
		// the ?v= hash makes the bytes cacheable forever.
		writeImmutable(w, v, http.StatusOK)
		_, _ = w.Write(raw)
	}
}
