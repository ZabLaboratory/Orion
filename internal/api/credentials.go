package api

import "net/http"

// getStreamKey reserves the legacy URL Prism's main process expects.
//
// Per ADR 004 § 11 the stream-key handover endpoint is preserved
// verbatim across the rewrite, but the credential storage that
// backed it (encrypted twitch_credentials table) was deleted with
// v0.x and now belongs to **Quasar** (ADR 005). Until Quasar's
// service is up and Orion v2 wires through to it, the endpoint
// returns 503 with a typed code so Prism's broadcast pre-flight
// surfaces a clear error rather than a generic failure.
//
// When Quasar lands, this handler swaps to a thin proxy that asks
// Quasar for the active operator's stream key.
func getStreamKey(_ PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"code":    "TWITCH_CREDENTIAL_UNAVAILABLE",
			"message": "credentials moved to Quasar (ADR 005); not yet wired",
		})
	})
}
