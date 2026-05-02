package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Poller runs HTTP-poll bindings declared by a scene's graph
// (ADR 004 § 8). One goroutine per (URL, frequency) tuple; failures
// flip __system.adapter_health.<key> to false; 429 yields
// exponential backoff until success.
type Poller struct {
	logger *slog.Logger
	client *http.Client
	inbox  *Inbox
	ua     string

	mu    sync.Mutex
	jobs  map[string]*pollerJob // key = adapter `key`
}

type pollerJob struct {
	cfg    compiler.ExternalAdapter
	scene  *runtime.Scene
	cancel context.CancelFunc
	done   chan struct{}
}

// NewPoller builds a Poller. userAgent is sent with every request.
func NewPoller(inbox *Inbox, logger *slog.Logger, userAgent string) *Poller {
	return &Poller{
		logger: logger.With("component", "poller"),
		client: &http.Client{Timeout: 10 * time.Second},
		inbox:  inbox,
		ua:     userAgent,
		jobs:   map[string]*pollerJob{},
	}
}

// Start scans a scene's graph for `http-poll` bindings and launches
// a goroutine per binding. Idempotent — re-calling Start for the
// same scene replaces the existing jobs (e.g., on a re-push).
func (p *Poller) Start(ctx context.Context, scene *runtime.Scene) {
	p.Stop(scene.ID())
	for _, b := range scene.Graph().Bindings {
		if b.Kind != "http-poll" || b.URL == "" || b.FrequencyHz == nil || *b.FrequencyHz <= 0 {
			continue
		}
		jobCtx, cancel := context.WithCancel(ctx)
		job := &pollerJob{
			cfg:    b,
			scene:  scene,
			cancel: cancel,
			done:   make(chan struct{}),
		}
		p.mu.Lock()
		p.jobs[scene.ID()+"|"+b.Key] = job
		p.mu.Unlock()
		go p.run(jobCtx, job)
	}
}

// Stop cancels all jobs for one scene.
func (p *Poller) Stop(sceneID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prefix := sceneID + "|"
	for k, j := range p.jobs {
		if strings.HasPrefix(k, prefix) {
			j.cancel()
			<-j.done
			delete(p.jobs, k)
		}
	}
}

// StopAll cancels every job (process shutdown).
func (p *Poller) StopAll() {
	p.mu.Lock()
	jobs := make([]*pollerJob, 0, len(p.jobs))
	for _, j := range p.jobs {
		jobs = append(jobs, j)
	}
	p.jobs = map[string]*pollerJob{}
	p.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
		<-j.done
	}
}

func (p *Poller) run(ctx context.Context, job *pollerJob) {
	defer close(job.done)

	interval := time.Duration(float64(time.Second) / *job.cfg.FrequencyHz)
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	healthPath := fmt.Sprintf("__system.adapter_health.%s", job.cfg.Key)

	backoff := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval + backoff):
		}

		val, err := p.fetch(ctx, job.cfg.URL)
		if err != nil {
			p.logger.Warn("poller fetch failed", "key", job.cfg.Key, "url", job.cfg.URL, "err", err)
			p.markHealth(job, healthPath, false)
			backoff = nextBackoff(backoff, errors.Is(err, errRateLimited))
			continue
		}
		backoff = 0
		p.markHealth(job, healthPath, true)

		// One write per declared target path. The compiler validated
		// that target_paths cover the leaves the layout expects.
		for _, tp := range job.cfg.TargetPaths {
			_ = p.inbox.Write(ctx, Write{
				Identity: auth.Identity{Role: auth.RoleService, UserID: "poller", Paths: []string{tp}},
				Path:     tp,
				Value:    val,
				Source:   "service:poller/" + job.cfg.Key,
				System:   true,
			})
		}
	}
}

var errRateLimited = errors.New("poller: rate limited")

func (p *Poller) fetch(ctx context.Context, url string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.ua)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, errRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

func (p *Poller) markHealth(job *pollerJob, path string, healthy bool) {
	val := json.RawMessage(`true`)
	if !healthy {
		val = json.RawMessage(`false`)
	}
	job.scene.Input(runtime.InputMsg{
		Path:     path,
		Value:    val,
		Source:   "system:poller-health",
		IsSystem: true,
	})
}

func nextBackoff(prev time.Duration, rateLimited bool) time.Duration {
	if !rateLimited {
		return 0
	}
	if prev == 0 {
		return time.Second
	}
	if prev >= 30*time.Second {
		return 30 * time.Second
	}
	return prev * 2
}
