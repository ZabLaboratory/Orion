package api

import "sync"

// pushDedup is the process-local idempotence cache the push handler holds
// across requests (switch fix): it maps a compile-input fingerprint
// (compiler.EnvelopeFingerprint) to the scene_version that input already
// compiled to. A go-live pushes the same scene twice (push → validate →
// re-push) and every compile re-fetches the Canvas layout + blueprints over
// the WAN; a fingerprint hit lets the second push skip Compile and every
// fetch entirely, resolving the stored pushed version instead.
//
// It is intentionally in-memory and process-local: the target is the
// back-to-back double push within one go-live (same process, milliseconds
// apart). A restart simply forgets the map — the first push after boot
// recompiles once (the disk fetch cache still spares its WAN cost), which
// is correct, never stale. The map is a lookup HINT only: the handler
// always re-resolves the pushed version from the store and falls back to a
// full compile if the row is gone (e.g. archived/purged), so a stale entry
// can never serve a wrong or absent artefact.
//
// Bounded FIFO eviction keeps it flat under an unbounded scene set; the
// working set of scenes a single show touches is tiny, so a small cap holds
// the hot go-live loop.
type pushDedup struct {
	mu       sync.Mutex
	capacity int
	m        map[string]string // fingerprint -> scene_version
	order    []string          // insertion order, oldest first, for eviction
}

func newPushDedup(capacity int) *pushDedup {
	if capacity < 1 {
		capacity = 1
	}
	return &pushDedup{capacity: capacity, m: make(map[string]string, capacity)}
}

// get returns the scene_version a fingerprint last compiled to, if known.
func (d *pushDedup) get(fingerprint string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.m[fingerprint]
	return v, ok
}

// put records fingerprint → sceneVersion, evicting the oldest entry when the
// cap is reached. A re-put of a known fingerprint just refreshes its value.
func (d *pushDedup) put(fingerprint, sceneVersion string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.m[fingerprint]; ok {
		d.m[fingerprint] = sceneVersion
		return
	}
	if len(d.order) >= d.capacity {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.m, oldest)
	}
	d.m[fingerprint] = sceneVersion
	d.order = append(d.order, fingerprint)
}

// drop removes a fingerprint whose stored version the store no longer has
// (archived / purged), so a later push recomputes it rather than looping on
// a dangling hint.
func (d *pushDedup) drop(fingerprint string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.m[fingerprint]; !ok {
		return
	}
	delete(d.m, fingerprint)
	for i, fp := range d.order {
		if fp == fingerprint {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
}
