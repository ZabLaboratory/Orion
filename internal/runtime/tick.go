package runtime

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// Tick is the process-wide source for time-based bindings (timers,
// schedulers). Per ADR 004 § 4.2 + tick.go note: a single goroutine
// runs at a fixed cadence (default 60 Hz from config). It writes
// __system.tick.now_ms into the ACTIVE scene only (ADR 008 §3.1): a
// dormant roster scene receives no tick, so it neither recomputes nor
// fires on-tick. The active scene that doesn't bind the tick pays zero
// cost (the recompute pass is dirty-driven).
type Tick struct {
	hz     int
	show   *Show
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// NewTick builds a tick source. Run() is non-blocking: launches the
// goroutine and returns immediately.
func NewTick(hz int, show *Show) *Tick {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tick{
		hz:     hz,
		show:   show,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

// Run starts the ticker goroutine.
func (t *Tick) Run() {
	go t.loop()
}

// Stop signals the loop to exit.
func (t *Tick) Stop() {
	t.cancel()
	<-t.done
}

// tickPath is the global tick leaf every scene receives (and the
// write the exec layer's `on-tick` trigger fires on — issue #83).
const tickPath = "__system.tick.now_ms"

func (t *Tick) loop() {
	defer close(t.done)
	if t.hz <= 0 {
		return
	}
	interval := time.Second / time.Duration(t.hz)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case now := <-ticker.C:
			payload := json.RawMessage(strconv.FormatInt(now.UnixMilli(), 10))
			t.fanout(InputMsg{
				Path:     tickPath,
				Value:    payload,
				Source:   "system:tick",
				IsSystem: true,
			})
		}
	}
}

// fanout writes the tick to the ACTIVE scene only (ADR 008 §3.1). The
// tick does NOT pass through the inbox — it routes here directly — so it
// must follow the active pointer on its own, exactly like Inbox.Write.
// A dormant roster scene receives no tick: zero recompute, zero on-tick
// fire. The active scene that doesn't read __system.tick from its graph
// never propagates it (the recompute pass is dirty-driven). A tick in
// flight during a switch lands on whichever scene was active at the
// Active() read — accepted, equivalent to a tick one frame earlier.
func (t *Tick) fanout(msg InputMsg) {
	if scene := t.show.Active(); scene != nil {
		scene.Input(msg)
	}
}
