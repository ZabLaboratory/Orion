package effects

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// TestRunner_DeliversOffCaller: a job runs on a worker goroutine and
// its Result reaches Deliver exactly once.
func TestRunner_DeliversOffCaller(t *testing.T) {
	r := NewRunner(2, 8, testLogger())
	r.Start()
	defer r.Stop()

	got := make(chan Result, 1)
	ok := r.Submit(Job{
		SceneID: "s1",
		Run:     func(context.Context) Result { return Result{Value: json.RawMessage(`42`)} },
		Deliver: func(res Result) { got <- res },
	})
	if !ok {
		t.Fatal("submit refused")
	}
	select {
	case res := <-got:
		if string(res.Value) != "42" || res.Err != "" {
			t.Fatalf("res = %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery")
	}
}

// TestRunner_TimeoutIsEffectSemantics: a job that overruns its
// per-effect timeout completes with an EFFECT_TIMEOUT error Result —
// the worker is released, nothing is killed.
func TestRunner_TimeoutIsEffectSemantics(t *testing.T) {
	r := NewRunner(1, 1, testLogger())
	r.Start()
	defer r.Stop()

	got := make(chan Result, 1)
	r.Submit(Job{
		SceneID: "s1",
		Timeout: 20 * time.Millisecond,
		Run: func(ctx context.Context) Result {
			<-ctx.Done()
			return Result{}
		},
		Deliver: func(res Result) { got <- res },
	})
	select {
	case res := <-got:
		if !strings.HasPrefix(res.Err, "EFFECT_TIMEOUT") {
			t.Fatalf("err = %q, want EFFECT_TIMEOUT prefix", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery")
	}
}

// TestRunner_QueueFullRefusesSubmit: the pool is BOUNDED — beyond the
// queue, Submit returns false (back-pressure the caller resolves to
// the error port, never an unbounded goroutine spawn).
func TestRunner_QueueFullRefusesSubmit(t *testing.T) {
	r := NewRunner(1, 1, testLogger())
	r.Start()
	defer r.Stop()

	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	// Occupy the single worker…
	r.Submit(Job{
		Run:     func(context.Context) Result { <-release; return Result{} },
		Deliver: func(Result) { wg.Done() },
	})
	// …fill the single queue slot (may take one scheduling beat for the
	// worker to pick up the first job)…
	deadline := time.Now().Add(time.Second)
	for !r.Submit(Job{Run: func(context.Context) Result { <-release; return Result{} }, Deliver: func(Result) {}}) {
		if time.Now().After(deadline) {
			t.Fatal("could not fill the queue slot")
		}
	}
	// …now the pool must refuse.
	if r.Submit(Job{Run: func(context.Context) Result { return Result{} }, Deliver: func(Result) {}}) {
		t.Fatal("submit beyond worker+queue capacity must be refused")
	}
	close(release)
	wg.Wait()
}

// TestRunner_SubmitAfterStopRefused: a stopped runner refuses instead
// of panicking the scene goroutine.
func TestRunner_SubmitAfterStopRefused(t *testing.T) {
	r := NewRunner(1, 1, testLogger())
	r.Start()
	r.Stop()
	if r.Submit(Job{Run: func(context.Context) Result { return Result{} }, Deliver: func(Result) {}}) {
		t.Fatal("submit after Stop must be refused")
	}
}
