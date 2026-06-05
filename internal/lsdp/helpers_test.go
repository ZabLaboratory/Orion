package lsdp

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func quietLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// kitState reads a paired kit scene's current state for the given
// scene id (test-only; the kit exposes SnapshotForConformance).
func (w *Wire) kitState(sceneID string) map[string]string {
	w.mu.Lock()
	sc, ok := w.scenes[sceneID]
	w.mu.Unlock()
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, v := range sc.SnapshotForConformance() {
		out[k] = string(v)
	}
	return out
}

// waitForState polls the kit store until it reflects want at path, or
// the deadline passes. It bridges the async gap between scene.Input and
// the mirror's Emit landing in the kit scene.
func waitForState(t *testing.T, w *Wire, scene *runtime.Scene, path, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := w.kitState(scene.ID())[path]; got == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("kit state %q never reached %q", path, want)
}
