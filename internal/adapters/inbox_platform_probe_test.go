package adapters

// Probe tests — platform scope at the inbox level (issue #84).
// Complements Forge's inbox_platform_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// TestInbox_PlatformScope_KickForbidden: a Quasar service token scoped
// to `__inputs.platform.twitch.*` attempting a kick.* write must receive
// ErrWriteForbidden at the inbox level — not silently absorbed, not
// forwarded to any scene. This tests the criterion-11 enforcement for
// kick.* which is omitted from Forge's TestInbox_PlatformServiceScope.
func TestInbox_PlatformScope_KickForbidden(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.Load("scene-pf", platformGraph("scene-pf"), &compiler.RenderBundle{SceneVersion: "sha256:test"})

	inbox := NewInbox(show, quietLogger(), nil)
	// Quasar token — twitch-scoped only.
	quasar := identityForTest("service", "quasar", []string{"__inputs.platform.twitch.*"})

	// kick.* is outside the twitch scope: must be WRITE_FORBIDDEN.
	err := inbox.Write(context.Background(), Write{
		Identity: quasar,
		Path:     "__inputs.platform.kick.zabchannel.last_chat",
		Value:    json.RawMessage(`{}`),
		Source:   "service:quasar",
	})
	if !errors.Is(err, ErrWriteForbidden) {
		t.Fatalf("kick.* write: err = %v, want ErrWriteForbidden (criterion 11 — kick is out-of-scope)", err)
	}
}

// TestInbox_PlatformScope_MultiPlatformToken: a token with BOTH twitch
// and kick scopes can write both, but not youtube — ensures the
// multi-scope path check is per-entry and does not short-circuit.
func TestInbox_PlatformScope_MultiPlatformToken(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)

	// Scene with twitch binding.
	show.Load("scene-pf", platformGraph("scene-pf"), &compiler.RenderBundle{SceneVersion: "sha256:test"})

	inbox := NewInbox(show, quietLogger(), nil)
	// Token with both twitch and kick scopes.
	multi := identityForTest("service", "quasar-multi",
		[]string{"__inputs.platform.twitch.*", "__inputs.platform.kick.*"})

	// twitch in-scope — must succeed (no scene for kick so it's a silent
	// no-op, but the write is not forbidden).
	err := inbox.Write(context.Background(), Write{
		Identity: multi,
		Path:     twitchLeaf,
		Value:    json.RawMessage(`{"n":1}`),
		Source:   "service:quasar-multi",
	})
	if err != nil {
		t.Fatalf("twitch write with multi-scope token: unexpected error %v", err)
	}

	// kick in-scope — must NOT be forbidden (even though no scene accepts it).
	err = inbox.Write(context.Background(), Write{
		Identity: multi,
		Path:     "__inputs.platform.kick.zabchannel.last_chat",
		Value:    json.RawMessage(`{}`),
		Source:   "service:quasar-multi",
	})
	if errors.Is(err, ErrWriteForbidden) {
		t.Fatal("kick write with multi-scope token: got WRITE_FORBIDDEN, kick is in scope")
	}

	// youtube out-of-scope — must be forbidden.
	err = inbox.Write(context.Background(), Write{
		Identity: multi,
		Path:     "__inputs.platform.youtube.zabchannel.last_chat",
		Value:    json.RawMessage(`{}`),
		Source:   "service:quasar-multi",
	})
	if !errors.Is(err, ErrWriteForbidden) {
		t.Fatalf("youtube write with multi-scope token: err = %v, want WRITE_FORBIDDEN", err)
	}
}
