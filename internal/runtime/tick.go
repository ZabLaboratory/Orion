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
// __system.tick.now_ms into every loaded scene that declares a
// binding on it. Scenes that don't bind it pay zero cost.
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

func (t *Tick) loop() {
	defer close(t.done)
	if t.hz <= 0 {
		return
	}
	interval := time.Second / time.Duration(t.hz)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	const path = "__system.tick.now_ms"
	for {
		select {
		case <-t.ctx.Done():
			return
		case now := <-ticker.C:
			payload := json.RawMessage(strconv.FormatInt(now.UnixMilli(), 10))
			t.fanout(InputMsg{
				Path:     path,
				Value:    payload,
				Source:   "system:tick",
				IsSystem: true,
			})
		}
	}
}

// fanout writes the tick to every scene that declared a binding on
// the tick path. v1: the binding-decl awareness lives in the graph's
// Bindings list. We iterate scenes and check their declared bindings.
func (t *Tick) fanout(msg InputMsg) {
	for _, id := range t.show.IDs() {
		scene, err := t.show.Get(id)
		if err != nil {
			continue
		}
		// In v1 we deliver the tick unconditionally; scenes that
		// don't read __system.tick from their graph never propagate
		// it (the recompute pass is dirty-driven). Cheap fan-out.
		scene.Input(msg)
	}
}
