package api

import (
	"net/http"
	"strings"
)

// staticSolarHandler serves the per-version Solar bundles from a
// configurable filesystem root. URL pattern:
//
//	/static/solar/v{N.N.N}/<file>
//
// Per ADR 003 § 8 + ADR 004 § 12 criterion 12, the response carries
// long-TTL immutable cache headers; the version sits in the path
// (not a query string) so a Solar upgrade does not invalidate live
// streams pinned to the previous version.
func staticSolarHandler(fs http.FileSystem) http.Handler {
	if fs == nil {
		// Fail closed so a misconfigured deploy returns 404 instead
		// of silently 200ing nothing.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "static root unconfigured", http.StatusNotFound)
		})
	}
	server := http.FileServer(fs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip the /static/solar/ prefix so the file server resolves
		// against the configured root.
		const prefix = "/static/solar/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		r2 := *r
		r2.URL.Path = r.URL.Path[len(prefix)-1:] // keep leading "/"
		server.ServeHTTP(w, &r2)
	})
}
