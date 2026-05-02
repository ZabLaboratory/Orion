package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// Criterion 6: a scene declaring a 5 Hz HTTP poll writes the polled
// value to the bound leaf and deltas come out at that frequency.
func TestPoller_5HzWritesAtCadence(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		_, _ = io.WriteString(w, fmt.Sprintf(`%d`, n))
	}))
	defer srv.Close()

	freq := 5.0
	graph := &compiler.Graph{
		SceneID:      "scene-poll",
		SceneVersion: "sha256:test",
		Nodes: []compiler.GraphNode{
			{ID: "in.score", Kind: "input"},
			{ID: "out.score", Kind: "output", Path: "score.team_a", Compute: "core.passthrough", Upstream: []string{"in.score"}},
		},
		Defaults: map[string]json.RawMessage{
			"score.team_a": json.RawMessage(`0`),
		},
		Bindings: []compiler.ExternalAdapter{
			{
				Key:         "test-poll",
				Kind:        "http-poll",
				URL:         srv.URL,
				FrequencyHz: &freq,
				TargetPaths: []string{"score.team_a"},
			},
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}
	scene := runtime.NewScene("scene-poll", graph, bundle, runtime.NewComputeRegistry(), quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	show.Load("scene-poll", graph, bundle)
	t.Cleanup(show.Stop)

	loaded, _ := show.Get("scene-poll")
	sub, _ := loaded.Subscribe(64)

	inbox := NewInbox(show, quietLogger())
	poller := NewPoller(inbox, quietLogger(), "test-ua/1")
	poller.Start(ctx, loaded)
	t.Cleanup(poller.StopAll)

	// 1 s window at 5 Hz → ~5 polls. Deltas may coalesce so we accept
	// any non-zero count and assert at least 3 over the window
	// (defending against scheduler jitter).
	deadline := time.After(1500 * time.Millisecond)
	deltas := 0
	for {
		select {
		case msg := <-sub.Out:
			if d, ok := msg.(*protocol.Delta); ok && len(d.Patches) > 0 {
				deltas++
			}
		case <-deadline:
			if deltas < 3 {
				t.Errorf("got %d deltas in 1.5s at 5 Hz, expected ≥ 3", deltas)
			}
			return
		}
	}
}

func TestInbox_RejectsTestNamespaceFromNonSystem(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	inbox := NewInbox(show, quietLogger())

	// Build identity that *can* write to __test.* (admin) but the
	// inbox still rejects on the namespace gate for live show writes.
	ident := identityForTest("operator", "user-x", nil)
	err := inbox.Write(context.Background(), Write{
		Identity: ident,
		Path:     "__test.tick",
		Value:    json.RawMessage(`123`),
		Source:   "operator:user-x",
	})
	if err == nil {
		t.Fatal("expected ErrWriteForbidden on __test.* in live mode")
	}
}
