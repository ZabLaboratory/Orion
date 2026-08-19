package adapters

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestInbox_RejectsTestNamespaceFromNonSystem(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	inbox := NewInbox(show, quietLogger(), nil)

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
