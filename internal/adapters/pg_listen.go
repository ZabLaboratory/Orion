package adapters

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// PGListener wires Postgres LISTEN/NOTIFY into the inbox per ADR 004
// § 8. A scene declares a binding of kind "pg-listen" with a channel
// name; the listener subscribes to that channel and writes the
// payload to the binding's target_paths.
type PGListener struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	inbox  *Inbox

	mu   sync.Mutex
	jobs map[string]*pgListenJob
}

type pgListenJob struct {
	cfg    compiler.ExternalAdapter
	scene  *runtime.Scene
	cancel context.CancelFunc
	done   chan struct{}
}

// NewPGListener builds a listener.
func NewPGListener(pool *pgxpool.Pool, inbox *Inbox, logger *slog.Logger) *PGListener {
	return &PGListener{
		pool:   pool,
		logger: logger.With("component", "pg_listen"),
		inbox:  inbox,
		jobs:   map[string]*pgListenJob{},
	}
}

// Start scans a scene's graph for pg-listen bindings and launches a
// goroutine per (channel) tuple. Idempotent.
func (l *PGListener) Start(ctx context.Context, scene *runtime.Scene) {
	l.Stop(scene.ID())
	for _, b := range scene.Graph().Bindings {
		if b.Kind != "pg-listen" || b.Channel == "" {
			continue
		}
		jobCtx, cancel := context.WithCancel(ctx)
		job := &pgListenJob{
			cfg:    b,
			scene:  scene,
			cancel: cancel,
			done:   make(chan struct{}),
		}
		l.mu.Lock()
		l.jobs[scene.ID()+"|"+b.Channel] = job
		l.mu.Unlock()
		go l.run(jobCtx, job)
	}
}

// Stop cancels every listen job for the given scene.
func (l *PGListener) Stop(sceneID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, j := range l.jobs {
		if len(k) >= len(sceneID)+1 && k[:len(sceneID)+1] == sceneID+"|" {
			j.cancel()
			<-j.done
			delete(l.jobs, k)
		}
	}
}

// StopAll cancels every job.
func (l *PGListener) StopAll() {
	l.mu.Lock()
	jobs := make([]*pgListenJob, 0, len(l.jobs))
	for _, j := range l.jobs {
		jobs = append(jobs, j)
	}
	l.jobs = map[string]*pgListenJob{}
	l.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
		<-j.done
	}
}

func (l *PGListener) run(ctx context.Context, job *pgListenJob) {
	defer close(job.done)

	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		l.logger.Error("pg_listen: acquire conn", "channel", job.cfg.Channel, "err", err)
		return
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `LISTEN `+pgIdent(job.cfg.Channel)); err != nil {
		l.logger.Error("pg_listen: LISTEN failed", "channel", job.cfg.Channel, "err", err)
		return
	}

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			l.logger.Warn("pg_listen: wait failed", "channel", job.cfg.Channel, "err", err)
			return
		}
		val := json.RawMessage(notification.Payload)
		if !json.Valid(val) {
			val, _ = json.Marshal(notification.Payload)
		}
		for _, tp := range job.cfg.TargetPaths {
			_ = l.inbox.Write(ctx, systemWrite(Write{
				Identity: auth.Identity{Role: auth.RoleService, UserID: "pg-listen", Paths: []string{tp}},
				Path:     tp,
				Value:    val,
				Source:   "service:pg-listen/" + job.cfg.Channel,
			}))
		}
	}
}

// pgIdent quotes a Postgres identifier safely for LISTEN. We accept
// identifiers consisting of alnum + underscore only — the compiler
// validates this at scene push time.
func pgIdent(name string) string {
	out := make([]byte, 0, len(name)+2)
	out = append(out, '"')
	for _, c := range []byte(name) {
		if c == '"' {
			out = append(out, '"', '"')
		} else {
			out = append(out, c)
		}
	}
	out = append(out, '"')
	return string(out)
}

// Ensure pgx is imported even if we don't use a top-level symbol —
// the listener references conn.Conn().WaitForNotification, which
// keeps the dependency clean. The blank imports are not needed.
var _ pgx.LargeObject
