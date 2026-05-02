package api

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// getAsset serves a content-addressed asset binary. The metadata row
// holds the filesystem path under cfg.AssetRoot; we open and stream
// the file. Filesystem path is whitelisted to AssetRoot to defend
// against path traversal in case the row was tampered.
func getAsset(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseUUID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid asset id"})
			return
		}
		asset, err := deps.Store.GetAsset(r.Context(), id)
		if err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}

		// Whitelist: resolved filesystem path must live under AssetRoot.
		root, err := filepath.Abs(deps.Config.AssetRoot)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		full, err := filepath.Abs(asset.FilesystemPath)
		if err != nil || !pathHasPrefix(full, root) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}

		f, err := os.Open(full)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		defer f.Close()
		stat, _ := f.Stat()

		w.Header().Set("Content-Type", asset.Mime)
		w.Header().Set("ETag", `"`+asset.SHA256Hex+`"`)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeContent(w, r, asset.SHA256Hex, stat.ModTime(), f)
	}
}

// pathHasPrefix is a tiny helper that compares two filesystem paths
// after canonicalising separators so a Windows backslash run doesn't
// fool the prefix check.
func pathHasPrefix(p, prefix string) bool {
	pSep := filepath.ToSlash(p) + "/"
	prefixSep := filepath.ToSlash(prefix) + "/"
	if len(pSep) < len(prefixSep) {
		return false
	}
	return pSep[:len(prefixSep)] == prefixSep
}

// silence unused import warnings if uuid drops out later.
var _ = uuid.Nil
