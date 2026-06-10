package runtime

import "time"

// Clock abstracts the time source the scene's timer wheel runs on
// (ADR 003 §3.1.3: "`delay` … parks with a wake key on the scene's
// timer wheel; tests use a fake clock"). The production implementation
// wraps the `time` package; tests inject a fake clock so `delay`
// semantics are proven deterministically — no real sleeps, no flaky
// wall-clock margins.
type Clock interface {
	// Now returns the current instant.
	Now() time.Time
	// NewTimer returns a timer that delivers on its channel once,
	// d from now. d <= 0 delivers immediately.
	NewTimer(d time.Duration) Timer
}

// Timer is the injectable subset of *time.Timer the wheel needs. With
// go.mod >= 1.23 the runtime timer semantics guarantee Stop/Reset
// never leave a stale delivery behind — the fake implementation in
// tests mirrors that contract.
type Timer interface {
	// C is the delivery channel. Consumed exclusively inside the
	// scene loop's select — never concurrently with scene state.
	C() <-chan time.Time
	// Stop disarms the timer; no delivery happens after Stop.
	Stop()
	// Reset re-arms the timer for d from now, superseding any
	// previous schedule.
	Reset(d time.Duration)
}

// systemClock is the production Clock: the real time package.
type systemClock struct{}

func (systemClock) Now() time.Time                { return time.Now() }
func (systemClock) NewTimer(d time.Duration) Timer { return &systemTimer{time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (t *systemTimer) C() <-chan time.Time  { return t.t.C }
func (t *systemTimer) Stop()                { t.t.Stop() }
func (t *systemTimer) Reset(d time.Duration) { t.t.Reset(d) }
