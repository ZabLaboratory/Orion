package api

import "net/http"

// getLSMLBundle serves the content-addressed LSML bundle bytes at
// GET /api/v1/scenes/{id}/lsml-bundle?v={scene_version} (ADR 007 §C.2).
//
// Migrated off Store (#15, #331): serves the SAME bluehost.Host-backed
// bytes as getRenderBundle now — in the new model there is only one
// bundle format per instance (ZabCanvas's lsml_bundle, §6.3), so
// render-bundle and lsml-bundle are no longer two artefacts derived from
// one compiler.RenderBundle (the legacy dual-emission this endpoint's
// original doc comment described); they serve identical bytes. Kept as
// a distinct route only because Solar/Prism hardcode both paths — same
// {id}-is-vestigial, ?v=-is-the-real-key posture as getRenderBundle.
//
// LSML_DISABLED (bespoke mode) no longer applies: the new path has no
// LSDP-mode gate — the bundle either exists (a slot is loaded) or 404s.
func getLSMLBundle(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveHostBundle(w, r, deps)
	}
}
