// Package effects implements the phase-3 async-effect executors of
// ADR 003 §3.1.3 (issue #85): a bounded worker pool that runs
// `http.request` / `db.query` / `source.read` OFF the scene goroutine,
// the fail-closed HTTP egress policy (anti-SSRF, post-DNS — R2/B1),
// and the topology-A `db.query` delegation client (`_query` via
// ZabGate — Amendment 1).
//
// The package never touches scene state: a job computes a Result and
// hands it to a Deliver callback; the runtime glue
// (runtime/exec_effects.go) turns that into an intra-process
// `scene.Input(InputMsg{ResumeExec, ResumeEnv})` — never a `__system.*`
// write. Doctrine §1.1: a timeout or a policy denial is EFFECT
// semantics (the `error` output port), never a task kill.
package effects

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// Result is the outcome of one effect execution. Exactly one of
// Value/Err is meaningful: Err != "" routes the resumed continuation
// down the effect's `error` output port.
type Result struct {
	Value json.RawMessage
	Err   string
}

// Job is one async effect to run off-goroutine.
type Job struct {
	// SceneID labels metrics/logs.
	SceneID string
	// Timeout bounds Run via its context — per-effect semantics
	// (ADR 003 §3.1.3), never a task kill: on expiry Run returns an
	// error Result and the continuation resumes down `error`.
	Timeout time.Duration
	// Run performs the effect. Must honour ctx.
	Run func(ctx context.Context) Result
	// Deliver receives the Result exactly once, from the worker
	// goroutine. The runtime glue forwards it to scene.Input.
	Deliver func(Result)
}

// Runner is the bounded worker pool. Workers and queue are fixed at
// construction; a Submit beyond the queue is refused (the caller
// resolves the effect to its `error` port — back-pressure, no kill).
type Runner struct {
	jobs   chan Job
	logger *slog.Logger

	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
	workers   int
}

// DefaultWorkers / DefaultQueue are the pool bounds when the env
// overrides are absent (ORION_EFFECT_WORKERS / ORION_EFFECT_QUEUE).
const (
	DefaultWorkers = 8
	DefaultQueue   = 256
)

// NewRunner builds a stopped pool. workers/queue <= 0 fall back to the
// defaults.
func NewRunner(workers, queue int, logger *slog.Logger) *Runner {
	if workers <= 0 {
		workers = DefaultWorkers
	}
	if queue <= 0 {
		queue = DefaultQueue
	}
	return &Runner{
		jobs:    make(chan Job, queue),
		logger:  logger.With("component", "effects"),
		workers: workers,
	}
}

// Start launches the workers. Idempotent.
func (r *Runner) Start() {
	r.startOnce.Do(func() {
		for i := 0; i < r.workers; i++ {
			r.wg.Add(1)
			go r.work()
		}
	})
}

// Stop closes the intake and waits for in-flight jobs to finish
// (effects are never killed — they complete into a stale resume if the
// scene moved on, which the version+epoch wake-key gate drops).
func (r *Runner) Stop() {
	r.stopOnce.Do(func() {
		close(r.jobs)
		r.wg.Wait()
	})
}

// Submit enqueues a job. Returns false when the queue is full (or the
// runner is stopped) — the caller fails the effect to its `error` port.
func (r *Runner) Submit(j Job) (ok bool) {
	defer func() {
		// Submit-after-Stop: the channel is closed — treat as refused
		// rather than panicking the scene goroutine.
		if recover() != nil {
			ok = false
		}
	}()
	select {
	case r.jobs <- j:
		return true
	default:
		return false
	}
}

func (r *Runner) work() {
	defer r.wg.Done()
	for j := range r.jobs {
		r.runOne(j)
	}
}

func (r *Runner) runOne(j Job) {
	ctx := context.Background()
	if j.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, j.Timeout)
		defer cancel()
	}
	res := j.Run(ctx)
	if ctx.Err() != nil {
		// The per-effect timeout expired (effect semantics — the error
		// port carries it; the task is never killed). Tag the result so
		// the author sees the timeout, whatever shape the underlying
		// transport error took.
		if res.Err == "" {
			res = Result{Err: "EFFECT_TIMEOUT: " + ctx.Err().Error()}
		} else {
			res.Err = "EFFECT_TIMEOUT: " + res.Err
		}
	}
	j.Deliver(res)
}
