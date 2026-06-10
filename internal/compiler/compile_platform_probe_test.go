package compiler

// Probe tests — platform leaf binding (issue #84).
// These complement Forge's compile_platform_test.go without rewriting it.
// Each case is independently reproducible and asserts a real invariant.

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestCompile_PlatformChannelCasefold_AllCaps: ALL-CAPS channel handle
// folds to lowercase before validation — `ZAB_CHANNEL` → `zab_channel`.
// The spec explicitly requires this variant (underscore-separated caps).
func TestCompile_PlatformChannelCasefold_AllCaps(t *testing.T) {
	bp := &BlueprintGraph{ID: "bp-1", Nodes: []BlueprintNode{
		platformNode("chat", "quasar.twitch.chat@1", "ZAB_CHANNEL"),
	}}
	g, err := compilePlatformBlueprint(t, bp)
	if err != nil {
		t.Fatalf("ZAB_CHANNEL should fold to zab_channel and succeed: %v", err)
	}
	const want = "__inputs.platform.twitch.zab_channel.last_chat"
	if g.Nodes[0].Path != want {
		t.Fatalf("ALL-CAPS fold: path = %q, want %q", g.Nodes[0].Path, want)
	}
}

// TestCompile_PlatformChannelDiagnostics_ExtraCases covers the cases
// that Forge's table omits:
//   - space in channel — fails INVALID (space is not in [a-z0-9_])
//   - JSON null for channel — fails INVALID (json.Unmarshal into string succeeds
//     but channel becomes "", so: MISSING, since empty == missing)
//   - Unicode with space — fails INVALID
func TestCompile_PlatformChannelDiagnostics_ExtraCases(t *testing.T) {
	cases := []struct {
		name string
		node BlueprintNode
		code DiagnosticCode
	}{
		{
			// A space is not in [a-z0-9_]; after casefold "zab channel" still
			// has a space — must be INVALID, not silently truncated or accepted.
			"space in channel",
			platformNode("n1", "quasar.twitch.chat@1", "zab channel"),
			ErrPlatformChannelInvalid,
		},
		{
			// JSON null unmarshals to empty string → treated as MISSING.
			"json null channel",
			BlueprintNode{ID: "n1", Compute: "quasar.twitch.chat@1",
				Config: map[string]json.RawMessage{"channel": json.RawMessage(`null`)}},
			ErrPlatformChannelMissing,
		},
		{
			// Unicode with space variant — covers non-ASCII + whitespace.
			"unicode with space",
			platformNode("n1", "quasar.twitch.chat@1", "zäb channel"),
			ErrPlatformChannelInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bp := &BlueprintGraph{ID: "bp-1", Nodes: []BlueprintNode{tc.node}}
			_, err := compilePlatformBlueprint(t, bp)
			if err == nil {
				t.Fatalf("%s: expected %s but compile succeeded", tc.name, tc.code)
			}
			var ce *CompileError
			if !errors.As(err, &ce) || !ce.HasCode(tc.code) {
				t.Fatalf("%s: expected %s, got %v", tc.name, tc.code, err)
			}
		})
	}
}

// TestCompile_PlatformLeaf_InjectionPrevented: a channel value that
// contains a dot (`.`) cannot forge a leaf that reaches an arbitrary
// segment — e.g. `"zab.last_raid"` must not produce a leaf of the
// form `...twitch.zab.last_raid.last_chat` which would be a path-
// injection bypass. The `^[a-z0-9_]+$` regex rejects it outright;
// this test pins the exact rejection path so it cannot silently regress.
func TestCompile_PlatformLeaf_InjectionPrevented(t *testing.T) {
	// Attempt: channel="zab.twitch.zabchannel.last_raid" (injects two
	// extra segments). After casefold the dot is still there — must fail.
	bp := &BlueprintGraph{ID: "bp-1", Nodes: []BlueprintNode{
		platformNode("n1", "quasar.twitch.chat@1", "zab.twitch.zabchannel.last_raid"),
	}}
	_, err := compilePlatformBlueprint(t, bp)
	if err == nil {
		t.Fatal("channel with dots must fail PLATFORM_CHANNEL_INVALID (injection vector)")
	}
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrPlatformChannelInvalid) {
		t.Fatalf("expected PLATFORM_CHANNEL_INVALID, got %v", err)
	}
}
