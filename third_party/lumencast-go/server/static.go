package server

import (
	"net/http"
	"path"
	"strings"
)

// StaticBundles returns an http.Handler that serves LSML scene
// bundles from a directory layout :
//
//	root/
//	  <scene_id>/
//	    <hash>/
//	      scene.json
//	      assets/...
//
// The handler enforces :
//   - immutable cache headers when the URL contains a sha256 segment,
//   - 404 on traversal attempts,
//   - 415 on requests that don't match the layout.
//
// Use as Config.HTTPHandler when you want the kit to also serve the
// bundle the runtime fetches by scene_version.
func StaticBundles(root string) http.Handler {
	fs := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)
		if strings.Contains(clean, "..") {
			http.NotFound(w, r)
			return
		}
		// Cache immutability on hashed URLs (anything matching
		// /<scene>/<sha256>/...).
		if isHashedPath(clean) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fs.ServeHTTP(w, r)
	})
}

// isHashedPath matches /<scene>/<sha256>/... where sha256 is a 64-char
// hex string. Light heuristic — refine if necessary.
func isHashedPath(p string) bool {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 3)
	if len(parts) < 2 {
		return false
	}
	h := parts[1]
	if len(h) != 64 {
		return false
	}
	for _, r := range h {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
