package api

import (
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"strings"
	"sync"
	"time"
)

// IdempotencyCache remembers the typed result of a scoped (principal,
// owner, tenant, stream, action, scene_digest, ref_id, idempotency_key)
// tuple — the minimum dedup key §6.4 requires. A replayed intent
// carrying the same tuple and a non-empty idempotency_key returns the
// cached result instead of re-running Prepare/Take and restarting the
// bridge. This mirrors ZabCanvas's own `issue_or_replay` idempotence
// pattern: the SAME request replayed is answered from the prior
// outcome, never re-executed.
//
// Bounded on two axes (ADR-BLUE-012 §12/B8, B3-R6-OPS-ORION — "fenêtres
// de replay/déduplication"; the principal risk named there is "surcharge
// non bornée"): a TTL retires an entry after ttl regardless of traffic
// (fail-open — outside the window a replay is processed fresh, matching
// §12's "no crash-safe durability promised" Gate-B replay posture), and
// maxEntries caps the map's size so a burst of distinct tuples cannot
// grow it without bound. Both are enforced in store, off the read path.
type IdempotencyCache struct {
	mu         sync.Mutex
	cache      map[string]idempotencyEntry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	metrics    IdempotencyMetrics
}

type idempotencyEntry struct {
	resp     sceneIntentResponse
	storedAt time.Time
}

// IdempotencyMetrics is the cache's observability seam. reason is "ttl"
// (lookup found an expired entry) or "capacity" (store evicted to stay
// at/under maxEntries). nil-safe: a nil cache.metrics disables counting,
// the bound itself still applies.
type IdempotencyMetrics interface {
	IdempotencyEvicted(reason string)
}

const (
	defaultIdempotencyTTL        = 60 * time.Second
	defaultIdempotencyMaxEntries = 4096
)

// NewIdempotencyCache builds a cache with the platform defaults (60s
// TTL, 4096 entries — see internal/config's ORION_IDEMPOTENCY_TTL_S /
// ORION_IDEMPOTENCY_MAX_ENTRIES documentation for the rationale).
func NewIdempotencyCache() *IdempotencyCache {
	return NewIdempotencyCacheWithLimits(defaultIdempotencyTTL, defaultIdempotencyMaxEntries, nil)
}

// NewIdempotencyCacheWithLimits builds a cache from explicit bounds —
// the production wiring path (cmd/orion), driven by config. ttl <= 0 or
// maxEntries <= 0 fall back to the platform default for that axis rather
// than disabling it: this cache has no documented unbounded mode.
func NewIdempotencyCacheWithLimits(ttl time.Duration, maxEntries int, metrics IdempotencyMetrics) *IdempotencyCache {
	if ttl <= 0 {
		ttl = defaultIdempotencyTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultIdempotencyMaxEntries
	}
	return &IdempotencyCache{
		cache:      map[string]idempotencyEntry{},
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
		metrics:    metrics,
	}
}

// setNowForTest injects the clock so a test can prove TTL expiry
// deterministically. TEST-ONLY by contract — no production path reaches it.
func (c *IdempotencyCache) setNowForTest(fn func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = fn
}

func (c *IdempotencyCache) lookup(key string) (sceneIntentResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok {
		return sceneIntentResponse{}, false
	}
	if c.now().Sub(e.storedAt) > c.ttl {
		delete(c.cache, key)
		if c.metrics != nil {
			c.metrics.IdempotencyEvicted("ttl")
		}
		return sceneIntentResponse{}, false
	}
	return e.resp, true
}

func (c *IdempotencyCache) store(key string, resp sceneIntentResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.cache[key] = idempotencyEntry{resp: resp, storedAt: now}
	if len(c.cache) <= c.maxEntries {
		return
	}
	// Over capacity: sweep TTL-expired entries first — the common case
	// under real traffic, where capacity pressure and staleness coincide.
	for k, e := range c.cache {
		if now.Sub(e.storedAt) > c.ttl {
			delete(c.cache, k)
			if c.metrics != nil {
				c.metrics.IdempotencyEvicted("ttl")
			}
		}
	}
	if len(c.cache) <= c.maxEntries {
		return
	}
	// Still over: evict the single oldest entry. O(n) scan — store is not
	// on the tick/fanout hot path (one call per scene-intent request), and
	// n is bounded by maxEntries itself.
	var oldestKey string
	var oldestAt time.Time
	first := true
	for k, e := range c.cache {
		if first || e.storedAt.Before(oldestAt) {
			oldestKey, oldestAt, first = k, e.storedAt, false
		}
	}
	if oldestKey != "" {
		delete(c.cache, oldestKey)
		if c.metrics != nil {
			c.metrics.IdempotencyEvicted("capacity")
		}
	}
}

// idempotencyKey builds the §6.4 minimum dedup tuple. Fields are
// NUL-joined (never user-controlled to contain NUL) rather than any
// separator that could appear in an id, so two distinct tuples can
// never collide by concatenation ambiguity.
func idempotencyKey(principal, owner, tenant, stream string, action attestation.Action, sceneDigest, refID, idempotencyKey string) string {
	return strings.Join([]string{principal, owner, tenant, stream, string(action), sceneDigest, refID, idempotencyKey}, "\x00")
}
