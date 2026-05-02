package api

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ZabLaboratory/Orion/internal/auth"
)

// getStreamKey is the operator-only handler for
// `GET /api/v1/credentials/{id}/stream-key`.
//
// History:
//   - v0.x : a local handler reading from Orion's encrypted
//     `twitch_credentials` table.
//   - v1.0.0 : that table was deleted with the Go rewrite ; the
//     endpoint stayed (Prism's main process consumes it through
//     `Prism/src/main/broadcast-engine.ts`) but returned
//     503 `TWITCH_CREDENTIAL_UNAVAILABLE` until Quasar landed.
//   - v1.1.0+ (this file) : thin proxy. Orion forwards the request to
//     `GET ${QUASAR_BASE_URL}/api/v1/credentials/{id}/stream-key`
//     (through ZabGate at `http://zabgate:4000/quasar/...`) carrying
//     a service-token Bearer header. Body and status are forwarded
//     verbatim so Prism's pre-flight surfaces the same error shapes
//     it would have got reading directly.
//
// ADR refs: 004 § 11 (preserved-verbatim endpoint), 005 § 11
// (stream-key handover into Quasar).
func getStreamKey(deps PublicDeps) http.HandlerFunc {
	client := &http.Client{Timeout: 10 * time.Second}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if deps.QuasarBaseURL == "" || deps.ServiceTokens == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"code":    "QUASAR_NOT_WIRED",
				"message": "ORION_QUASAR_BASE_URL or service token manager not configured",
			})
			return
		}
		accountID := r.PathValue("id")
		if accountID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST", "message": "account id required"})
			return
		}

		upstream := deps.QuasarBaseURL + "/api/v1/credentials/" + accountID + "/stream-key"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, nil)
		if err != nil {
			logger.Error("stream-key proxy: build request", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		req.Header.Set("Authorization", "Bearer "+deps.ServiceTokens.Token())
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			logger.Warn("stream-key proxy: upstream call failed", "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"code":    "QUASAR_UNREACHABLE",
				"message": "could not reach Quasar via gateway",
			})
			return
		}
		defer resp.Body.Close()

		// Forward content-type ; do not forward upstream's hop-by-hop
		// headers. Status + body land verbatim.
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			logger.Warn("stream-key proxy: copy body", "err", err)
		}
	})
}

// Compile-time assertion that the manager type matches what we depend
// on. Keeps the import alive if the call site changes shape later.
var _ = (*auth.ServiceTokenManager)(nil)
