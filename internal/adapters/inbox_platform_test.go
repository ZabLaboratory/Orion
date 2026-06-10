package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Platform-ingestion tests (ADR 003 §3.3, issue #84): the synthesized
// `platform-stream` binding makes sceneAcceptsPath accept Quasar's
// write (criterion 8), the service-token scope gates the platform
// namespace (criterion 11 E3, Orion half), and a refused scene.Input
// is counted (criterion 11 E2, Orion half).

const twitchLeaf = "__inputs.platform.twitch.zabchannel.last_chat"

// platformGraph mirrors what the compiler now emits for a
// `quasar.twitch.chat@1` node bound to channel `zabchannel`: an input
// node on the global leaf plus the synthesized acceptance binding.
func platformGraph(sceneID string) *compiler.Graph {
	return &compiler.Graph{
		SceneID:      sceneID,
		SceneVersion: "sha256:test",
		Nodes: []compiler.GraphNode{
			{ID: "chat", Kind: "input", Path: twitchLeaf, Compute: "quasar.twitch.chat@1"},
		},
		Defaults: map[string]json.RawMessage{},
		Bindings: []compiler.ExternalAdapter{
			{
				Key:         twitchLeaf,
				Label:       "Quasar platform stream",
				Kind:        "platform-stream",
				TargetPaths: []string{twitchLeaf},
			},
		},
	}
}

// TestSceneAcceptsPath_PlatformStreamBinding: the criterion-8 accept
// half — without the synthesized binding the write was silently
// absorbed; with it the leaf is accepted, and ONLY that leaf.
func TestSceneAcceptsPath_PlatformStreamBinding(t *testing.T) {
	scene := runtime.NewScene("scene-pf", platformGraph("scene-pf"),
		&compiler.RenderBundle{SceneVersion: "sha256:test"}, runtime.NewComputeRegistry(), quietLogger())

	if !sceneAcceptsPath(scene, twitchLeaf, false) {
		t.Fatal("platform leaf refused despite the platform-stream binding — Quasar's write would be silently absorbed")
	}
	if sceneAcceptsPath(scene, "__inputs.platform.twitch.zabchannel.last_raid", false) {
		t.Fatal("an unbound platform leaf must stay refused (accept set too broad)")
	}
}

// TestPlatformStreamBinding_SpawnsNoAdapterGoroutine: platform-stream
// is a PURE acceptance binding — neither the HTTP poller nor the PG
// listener may register a job (let alone a goroutine) for it.
func TestPlatformStreamBinding_SpawnsNoAdapterGoroutine(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.Load("scene-pf", platformGraph("scene-pf"), &compiler.RenderBundle{SceneVersion: "sha256:test"})
	scene, _ := show.Get("scene-pf")

	inbox := NewInbox(show, quietLogger(), nil)

	poller := NewPoller(inbox, quietLogger(), "test-ua/1")
	poller.Start(context.Background(), scene)
	t.Cleanup(poller.StopAll)
	poller.mu.Lock()
	pollJobs := len(poller.jobs)
	poller.mu.Unlock()
	if pollJobs != 0 {
		t.Fatalf("poller registered %d job(s) for a platform-stream binding, want 0", pollJobs)
	}

	listener := NewPGListener(nil, inbox, quietLogger())
	listener.Start(context.Background(), scene)
	t.Cleanup(listener.StopAll)
	listener.mu.Lock()
	listenJobs := len(listener.jobs)
	listener.mu.Unlock()
	if listenJobs != 0 {
		t.Fatalf("pg listener registered %d job(s) for a platform-stream binding, want 0", listenJobs)
	}
}

// TestInbox_PlatformServiceScope is criterion 11 E3, Orion half: a
// service token scoped `__inputs.platform.twitch.*` (what Quasar#9
// mints) writes the twitch leaf through to a subscriber delta, while
// `__inputs.platform.youtube.*` is refused with the write-forbidden
// error BEFORE any scene routing.
func TestInbox_PlatformServiceScope(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.Load("scene-pf", platformGraph("scene-pf"), &compiler.RenderBundle{SceneVersion: "sha256:test"})
	scene, _ := show.Get("scene-pf")
	sub, _ := scene.Subscribe(16)

	inbox := NewInbox(show, quietLogger(), nil)
	quasar := identityForTest("service", "quasar", []string{"__inputs.platform.twitch.*"})

	// In-scope write: accepted and observable.
	err := inbox.Write(context.Background(), Write{
		Identity: quasar,
		Path:     twitchLeaf,
		Value:    json.RawMessage(`{"message":"hello","user":"zab"}`),
		Source:   "service:quasar",
	})
	if err != nil {
		t.Fatalf("in-scope twitch write refused: %v", err)
	}
	deadline := time.After(2 * time.Second)
	delivered := false
	for !delivered {
		select {
		case msg := <-sub.Out:
			if d, ok := msg.(*protocol.Delta); ok && len(d.Patches) > 0 {
				if d.Patches[0].Path != twitchLeaf {
					t.Fatalf("delta path = %q, want %q", d.Patches[0].Path, twitchLeaf)
				}
				delivered = true
			}
		case <-deadline:
			t.Fatal("no delta observed for the in-scope platform write")
		}
	}

	// Out-of-scope platform: refused (criterion 11's youtube case).
	err = inbox.Write(context.Background(), Write{
		Identity: quasar,
		Path:     "__inputs.platform.youtube.zabchannel.last_chat",
		Value:    json.RawMessage(`{}`),
		Source:   "service:quasar",
	})
	if !errors.Is(err, ErrWriteForbidden) {
		t.Fatalf("youtube write: err = %v, want ErrWriteForbidden", err)
	}
}

// countingMetrics is the test double for the InboxMetrics seam.
type countingMetrics struct{ drops atomic.Int64 }

func (c *countingMetrics) InboxDropped(string) { c.drops.Add(1) }

// warnCounter counts WARN-level records (rate-limit assertion).
type warnCounter struct{ warns atomic.Int64 }

func (w *warnCounter) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (w *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.warns.Add(1)
	}
	return nil
}
func (w *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnCounter) WithGroup(string) slog.Handler      { return w }

// TestInbox_DropMetricAndRateLimitedWarn is criterion 11 E2, Orion
// half: when a scene's event loop refuses a write (full inbox
// channel) the inbox consumes scene.Input's return —
// `orion_inbox_dropped_total` counts EVERY drop, while the warn log
// is rate-limited (a flood of drops must not become a log flood).
func TestInbox_DropMetricAndRateLimitedWarn(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.Load("scene-pf", platformGraph("scene-pf"), &compiler.RenderBundle{SceneVersion: "sha256:test"})
	scene, _ := show.Get("scene-pf")
	// Halt the event loop: subsequent writes pile into the scene's
	// buffered channel until Input returns false — the refusal path.
	scene.Stop()

	counter := &countingMetrics{}
	warns := &warnCounter{}
	inbox := NewInbox(show, slog.New(warns), counter)
	quasar := identityForTest("service", "quasar", []string{"__inputs.platform.twitch.*"})

	const writes = 300 // scene inbox buffers 256 → ≥ 44 guaranteed drops
	for i := 0; i < writes; i++ {
		if err := inbox.Write(context.Background(), Write{
			Identity: quasar,
			Path:     twitchLeaf,
			Value:    json.RawMessage(`{"n":1}`),
			Source:   "service:quasar",
		}); err != nil {
			t.Fatalf("write %d: unexpected refusal %v (drops are not write errors)", i, err)
		}
	}

	if got := counter.drops.Load(); got < 1 {
		t.Fatal("no drop counted — scene.Input's return is still being discarded")
	}
	// All drops land within far less than dropWarnInterval, so the
	// rate limit allows exactly one warn for the whole burst.
	if got := warns.warns.Load(); got != 1 {
		t.Fatalf("warns = %d for a sub-interval burst, want exactly 1 (rate-limited)", got)
	}
}
