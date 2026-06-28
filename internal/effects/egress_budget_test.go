package effects

import (
	"sync"
	"testing"
	"time"
)

// TestStreamEgressLimiter_BurstThenDeny: a fresh stream passes exactly
// `perWindow` calls in a burst, then denies — the bound the R3 condition
// requires.
func TestStreamEgressLimiter_BurstThenDeny(t *testing.T) {
	base := time.Unix(1000, 0)
	l := NewStreamEgressLimiter(3, 10)
	l.setNowForTest(func() time.Time { return base }) // frozen — no refill

	for i := 0; i < 3; i++ {
		if !l.Allow("live") {
			t.Fatalf("call %d denied, want allowed within burst of 3", i+1)
		}
	}
	if l.Allow("live") {
		t.Fatal("4th call allowed, want denied past the budget")
	}
}

// TestStreamEgressLimiter_Isolation: a saturated stream does NOT affect
// another stream — each key owns its own bucket (the per-stream isolation
// RC). One blueprint hammering its show can never drain another show's
// (or a test session's) budget.
func TestStreamEgressLimiter_Isolation(t *testing.T) {
	base := time.Unix(2000, 0)
	l := NewStreamEgressLimiter(2, 10)
	l.setNowForTest(func() time.Time { return base })

	// Saturate stream A (budget of 2).
	for i := 0; i < 2; i++ {
		if !l.Allow("stream-a") {
			t.Fatalf("stream-a call %d denied, want allowed within burst", i+1)
		}
	}
	if l.Allow("stream-a") {
		t.Fatal("stream-a should be saturated")
	}
	// Stream B is untouched — full budget available.
	for i := 0; i < 2; i++ {
		if !l.Allow("stream-b") {
			t.Fatalf("stream-b call %d denied — must be unaffected by stream-a saturation", i+1)
		}
	}
	if l.Allow("stream-b") {
		t.Fatal("stream-b should now be at its own budget")
	}
}

// TestStreamEgressLimiter_Refill: tokens replenish at rate over time. A
// saturated stream recovers exactly one token per (window/perWindow)
// seconds — a legitimate long show keeps flowing, a runaway loop stays
// cut.
func TestStreamEgressLimiter_Refill(t *testing.T) {
	now := time.Unix(3000, 0)
	l := NewStreamEgressLimiter(2, 10) // rate = 0.2 tokens/s → 5 s per token
	l.setNowForTest(func() time.Time { return now })

	l.Allow("live")
	l.Allow("live")
	if l.Allow("live") {
		t.Fatal("expected saturation after the burst")
	}
	// 5 s → exactly one token back.
	now = now.Add(5 * time.Second)
	if !l.Allow("live") {
		t.Fatal("expected one refilled token after 5 s")
	}
	if l.Allow("live") {
		t.Fatal("only one token should have refilled")
	}
}

// TestStreamEgressLimiter_RefillCapsAtBurst: idle time never lets a bucket
// exceed its capacity (no saved-up burst beyond `burst`).
func TestStreamEgressLimiter_RefillCapsAtBurst(t *testing.T) {
	now := time.Unix(4000, 0)
	l := NewStreamEgressLimiter(2, 10)
	l.setNowForTest(func() time.Time { return now })

	l.Allow("live") // create + charge → 1 token left
	now = now.Add(1 * time.Hour)
	// Huge idle: refill is capped at burst (2), so only 2 calls pass.
	for i := 0; i < 2; i++ {
		if !l.Allow("live") {
			t.Fatalf("post-idle call %d denied, want allowed up to the burst cap", i+1)
		}
	}
	if l.Allow("live") {
		t.Fatal("a third call must be denied — refill caps at burst, no saved-up surplus")
	}
}

// TestStreamEgressLimiter_Disabled: perWindow <= 0 disables the bound
// (allow-all) — the documented opt-out. A nil limiter behaves the same.
func TestStreamEgressLimiter_Disabled(t *testing.T) {
	l := NewStreamEgressLimiter(0, 10)
	for i := 0; i < 1000; i++ {
		if !l.Allow("live") {
			t.Fatalf("disabled limiter denied call %d", i)
		}
	}
	var nilLimiter *StreamEgressLimiter
	if !nilLimiter.Allow("live") {
		t.Fatal("nil limiter must allow")
	}
}

// TestStreamEgressLimiter_ConcurrentSharedKey: live scenes of one show
// share a stream key on distinct goroutines — the limiter is race-clean
// and enforces ONE global budget across them (run under -race).
func TestStreamEgressLimiter_ConcurrentSharedKey(t *testing.T) {
	base := time.Unix(5000, 0)
	l := NewStreamEgressLimiter(100, 10)
	l.setNowForTest(func() time.Time { return base }) // frozen — no refill

	var mu sync.Mutex
	allowed := 0
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if l.Allow("live") {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	// 8*50 = 400 attempts on a frozen 100-token bucket → exactly 100 pass.
	if allowed != 100 {
		t.Fatalf("allowed = %d, want exactly 100 (shared per-stream budget)", allowed)
	}
}
