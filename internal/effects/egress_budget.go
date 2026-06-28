package effects

import (
	"sync"
	"time"
)

// Per-stream egress budget / rate-limit on curated `core.service.call@1`
// effects (ADR Blue 009 Amendment 2 §B, item 9 — the R3 condition of the
// G0 Bastion clearance). The threat: the assign-slot node writes on the
// live antenna loop; without a bound a compromised scene/blueprint (or a
// chat feedback loop) could hammer a downstream service (ZabCam) through
// the curated route. This is the bound, applied per STREAM (the Orion
// execution context — the live show, or an isolated preview / test
// session), so one saturated stream never starves another.
//
// Mechanism: a token bucket per stream key. The bucket holds at most
// `burst` tokens and refills at `rate` tokens/second. Each service.call
// spends one token; an empty bucket DENIES — the node fails closed to its
// `error` port (EGRESS_BUDGET_EXCEEDED), never a crash, never a blocked
// tick. The denial is pure effect semantics, evaluated synchronously on
// the scene goroutine before any worker job is submitted.

// StreamEgressLimiter is the per-stream token-bucket limiter. The zero
// value is unusable; build it with NewStreamEgressLimiter. A nil limiter
// allows everything (dev / unconfigured — production always wires one).
// Safe for concurrent use: live scenes of one show share a key and run on
// distinct goroutines, so the map and every bucket are mutex-guarded.
type StreamEgressLimiter struct {
	rate  float64 // tokens refilled per second (perWindow / windowSeconds)
	burst float64 // bucket capacity and initial fill (perWindow)

	// now is the clock — time.Now in prod, injected by tests to prove
	// refill deterministically without real sleeps.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*egressBucket
}

type egressBucket struct {
	tokens float64
	last   time.Time
}

// NewStreamEgressLimiter builds the limiter from the étage-1 budget: at
// most `perWindow` calls per `windowSeconds`-second window, per stream.
// The bucket capacity (burst) is `perWindow` and it refills at
// perWindow/windowSeconds tokens per second.
//
//   - perWindow <= 0 disables the limiter (Allow always true). Production
//     wiring passes a positive documented default; an operator can set 0
//     to opt out (logged at boot).
//   - windowSeconds <= 0 is treated as a pure fixed budget with NO refill:
//     the bucket drains to zero over the process lifetime and never
//     replenishes (defensive — config validation keeps it > 0 in prod).
func NewStreamEgressLimiter(perWindow, windowSeconds int) *StreamEgressLimiter {
	rate := 0.0
	if windowSeconds > 0 && perWindow > 0 {
		rate = float64(perWindow) / float64(windowSeconds)
	}
	return &StreamEgressLimiter{
		rate:    rate,
		burst:   float64(perWindow),
		now:     time.Now,
		buckets: map[string]*egressBucket{},
	}
}

// Allow charges one egress call against the given stream's bucket. It
// returns true (and spends a token) when the bucket has at least one
// token, false otherwise. A nil or disabled limiter always allows.
func (l *StreamEgressLimiter) Allow(streamKey string) bool {
	if l == nil || l.burst <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[streamKey]
	if !ok {
		// First call on this stream: a full bucket, immediately charged.
		l.buckets[streamKey] = &egressBucket{tokens: l.burst - 1, last: now}
		return true
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// setNowForTest injects the clock so a test can prove refill without real
// time passing. TEST-ONLY by contract — no production path reaches it.
func (l *StreamEgressLimiter) setNowForTest(fn func() time.Time) {
	l.now = fn
}
