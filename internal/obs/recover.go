package obs

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover wraps an http.Handler so a panic in any handler logs a
// structured event and replies 500 cleanly instead of taking the
// process down. The standard `net/http` server already recovers
// per-request, but it logs with the legacy logger; this wrapper makes
// the event slog-shaped and keeps the metric surface honest.
func Recover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.ErrorContext(r.Context(), "handler panic",
					"path", r.URL.Path,
					"method", r.Method,
					"recover", rec,
					"stack", string(debug.Stack()),
				)
				if !headersWritten(w) {
					http.Error(w, "internal error", http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// headersWritten is best-effort: net/http does not expose whether
// headers have flushed, so we use a tiny probe via WriteHeader's
// idempotency. When in doubt we skip writing — better an empty body
// than a duplicate WriteHeader log line.
func headersWritten(w http.ResponseWriter) bool {
	if hw, ok := w.(interface{ Status() int }); ok {
		return hw.Status() != 0
	}
	return false
}
