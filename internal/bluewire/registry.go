package bluewire

import (
	"context"
	"sync"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// Registry tracks the running Bridge, if any, for each bluehost.Slot and
// guarantees at most one is ever stepping a given slot at a time.
//
// Replacement policy (§6.7 idempotence/replacement, resolved here): a
// slot has AT MOST ONE bridge, ever. Starting a new bridge for a slot
// that already has one running (a Take superseding the on-air instance,
// or a re-Prepare) STOPS the previous one first — new replaces old,
// never coexists. This is the deliberate, minimal policy: no
// generation counter, no drain-then-swap, no dual-write window. It
// mirrors bluehost.Host's own single-instance-per-slot invariant one
// layer up, so the bridge lifecycle can never diverge from the
// instance lifecycle it steps. The alternative (letting two bridges
// briefly coexist on one slot to drain in-flight projections) was
// rejected: it would let a stale instance's output race a fresh one's
// onto the same LSDP scene, which is strictly worse than the one-frame
// gap a hard stop-then-start produces.
//
// Idempotent REQUEST replay (distinct from this replacement policy) is
// handled one layer up, in internal/api's IdempotencyCache: a repeated
// scene-intent request with the same dedup tuple returns the prior
// typed result without calling Start again at all — Start's own
// stop-then-start behavior here is for a genuinely NEW instance
// superseding an old one, not for replaying the same request.
type Registry struct {
	mu      sync.Mutex
	opMu    sync.Mutex
	running map[bluehost.Slot]*bridgeRun
}

type bridgeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewRegistry builds an empty Registry.
func NewRegistry() *Registry {
	return &Registry{running: map[bluehost.Slot]*bridgeRun{}}
}

// Start stops whatever bridge currently owns slot (if any) and starts
// bridge's Run loop in its own goroutine on interval. The previous run is
// cancelled and JOINED before the replacement is published, so a stale
// bridge cannot keep stepping a slot after a generation swap. onError is the
// caller's per-step error sink (e.g. a logger call) — Run itself never aborts
// the loop on a step error.
func (r *Registry) Start(slot bluehost.Slot, bridge *Bridge, interval time.Duration, onError func(error)) {
	if bridge == nil {
		return
	}
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}

	// Start/Stop/StopAll are serialized so a concurrent replacement cannot
	// publish an older generation after a newer one has already started.
	r.opMu.Lock()
	defer r.opMu.Unlock()

	r.mu.Lock()
	previous := r.running[slot]
	delete(r.running, slot)
	if previous != nil {
		previous.cancel()
	}
	r.mu.Unlock()
	if previous != nil {
		<-previous.done
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := &bridgeRun{cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	r.running[slot] = run
	r.mu.Unlock()
	go func() {
		defer close(run.done)
		bridge.Run(ctx, interval, onError)
	}()
}

// Stop stops slot's bridge, if any, and forgets it. Safe to call on a
// slot with no running bridge (no-op) — a Release path never needs its own
// existence check. Stop waits for the cancelled loop, making cleanup
// deterministic for Host.Release and generation replacement.
func (r *Registry) Stop(slot bluehost.Slot) {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	r.mu.Lock()
	run := r.running[slot]
	delete(r.running, slot)
	if run != nil {
		run.cancel()
	}
	r.mu.Unlock()
	if run != nil {
		<-run.done
	}
}

// Running reports whether slot currently has a bridge running — test
// seam, not used on any hot path.
func (r *Registry) Running(slot bluehost.Slot) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.running[slot]
	return ok
}

// StopAll stops every running bridge. Intended for process shutdown.
func (r *Registry) StopAll() {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	r.mu.Lock()
	runs := make([]*bridgeRun, 0, len(r.running))
	for slot, run := range r.running {
		runs = append(runs, run)
		run.cancel()
		delete(r.running, slot)
	}
	r.mu.Unlock()
	for _, run := range runs {
		<-run.done
	}
}
