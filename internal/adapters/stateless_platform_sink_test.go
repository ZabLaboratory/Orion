package adapters

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func TestInboxPlatformEventSinkReceivesAuthorizedPlatformWrite(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	inbox := NewInbox(show, quietLogger(), nil)
	var gotPath string
	var gotPayload any
	inbox.SetPlatformEventSink(func(path string, payload any) {
		gotPath = path
		gotPayload = payload
	})

	identity := identityForTest("service", "quasar", []string{"__inputs.platform.twitch.*"})
	const leaf = "__inputs.platform.twitch.g2nmathias.last_chat"
	if err := inbox.Write(context.Background(), Write{
		Identity: identity,
		Path:     leaf,
		Value:    json.RawMessage(`{"payload":{"text":"chat-proof"}}`),
		Source:   "service:quasar",
	}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if gotPath != leaf {
		t.Fatalf("sink path = %q, want %q", gotPath, leaf)
	}
	if gotPayload == nil {
		t.Fatal("sink payload is nil")
	}
}
