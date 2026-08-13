package bluewire

import (
	"context"
	"sync"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// Registry tracks the running Bridge, if any, for each bluehost.Slot and
// guarantees at most one is ever stepping a given slot at a time. A
// caller starting a new Bridge for a slot that already has one running
// (a Take superseding the on-air instance, or a re-Prepare) gets the
// PREVIOUS bridge stopped first — otherwise the old goroutine would keep
// calling Step on an instance bluehost.Host.Take has already released,
// erroring forever instead of exiting cleanly.
type Registry struct {
	mu     sync.Mutex
	cancel map[bluehost.Slot]context.CancelFunc
}

// NewRegistry builds an empty Registry.
func NewRegistry() *Registry {
	return &Registry{cancel: map[bluehost.Slot]context.CancelFunc{}}
}

// Start stops whatever bridge currently owns slot (if any) and starts
// bridge's Run loop in its own goroutine on interval. onError is the
// caller's per-step error sink (e.g. a logger call) — Run itself never
// aborts the loop on a step error.
func (r *Registry) Start(slot bluehost.Slot, bridge *Bridge, interval time.Duration, onError func(error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel, ok := r.cancel[slot]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel[slot] = cancel
	go bridge.Run(ctx, interval, onError)
}

// Stop stops slot's bridge, if any, and forgets it. Safe to call on a
// slot with no running bridge (no-op) — a Release path never needs its
// own existence check.
func (r *Registry) Stop(slot bluehost.Slot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cancel, ok := r.cancel[slot]; ok {
		cancel()
		delete(r.cancel, slot)
	}
}

// Running reports whether slot currently has a bridge running — test
// seam, not used on any hot path.
func (r *Registry) Running(slot bluehost.Slot) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.cancel[slot]
	return ok
}

// StopAll stops every running bridge. Intended for process shutdown.
func (r *Registry) StopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for slot, cancel := range r.cancel {
		cancel()
		delete(r.cancel, slot)
	}
}
