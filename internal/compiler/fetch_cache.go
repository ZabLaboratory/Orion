package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// fetchCache is a best-effort, content-addressed on-disk cache for the
// compiler's upstream fetches (the Canvas layout at GET /layouts/{hash} and
// the pinned Blue blueprint graph at /blueprints/{id}/versions/{n}/graph).
// A key maps to the same bytes forever, so an entry never needs eviction or a
// TTL: a changed artefact carries a new key.
//
// CAVEAT: the layout hash addresses the RAW server-side bundle, not the adapted
// response ZabCanvas actually serves — a server-side serialisation change
// (`adapt_bundle_to_layout`) can alter the bytes for a same hash. The layout
// key therefore embeds `layoutContractVersion` (see http_fetcher.go), bumped in
// lock-step with that adapter, so a shape change carries a NEW key rather than a
// silent stale hit. The blueprint-graph key stays purely content-addressed
// (the pinned version is immutable end-to-end).
//
// Motivation (switch fix): every scene push recompiles, and each compile
// re-fetches the layout + blueprints over the WAN against the prod gateway
// with no cache — the dominant cost of the ~3 s go-live switch. Caching the
// content-addressed responses on the service's data dir turns the second
// (and every later) fetch of the same hash into a local file read.
//
// It is deliberately best-effort: any filesystem error (unwritable dir,
// partial read) is swallowed and treated as a cache miss / skipped write, so
// the cache can never fail a compile — the fetcher just falls back to the
// network. A zero-value fetchCache (dir == "") is disabled: every Get misses
// and every Put is a no-op, which is the path unit tests and the antenne
// default without a configured cache dir take.
type fetchCache struct {
	dir string
}

// get returns the cached bytes for key and true, or nil/false on any miss
// (disabled cache, absent entry, or read error).
func (c fetchCache) get(key string) ([]byte, bool) {
	if c.dir == "" {
		return nil, false
	}
	b, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, false
	}
	return b, true
}

// put writes bytes for key, best-effort. A create/write error is ignored —
// the next fetch simply misses and re-fetches. The write is atomic (temp +
// rename) so a concurrent reader never sees a torn file.
func (c fetchCache) put(key string, b []byte) {
	if c.dir == "" {
		return
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return
	}
	final := c.path(key)
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
	}
}

// path hashes the key into a flat filename so an arbitrary key (a
// "sha256:<hex>" version, a "bpgraph:<id>:<n>" tuple) never escapes the
// cache dir or trips a filesystem charset limit.
func (c fetchCache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".json")
}
