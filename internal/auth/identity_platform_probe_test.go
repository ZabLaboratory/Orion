package auth

// Probe tests — criterion 11 scope enforcement at matchPath level (issue #84).
// Complements the matchPath table Forge added to identity_test.go.

import "testing"

// TestMatchPath_PlatformScope_KickAndYoutube pins that a Quasar token
// scoped to twitch.* CANNOT reach kick.* or youtube.*, ensuring the
// fence is per-platform, not per-namespace. `kick.*` is absent from
// Forge's table and must stay denied.
func TestMatchPath_PlatformScope_KickAndYoutube(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
		label   string
	}{
		// Quasar twitch token vs. kick and youtube (must all be false).
		{"__inputs.platform.twitch.*", "__inputs.platform.kick.zabchannel.last_chat", false, "twitch token vs kick — must deny"},
		{"__inputs.platform.twitch.*", "__inputs.platform.kick.any.last_follow", false, "twitch token vs kick.follow — must deny"},
		{"__inputs.platform.twitch.*", "__inputs.platform.youtube.any.last_chat", false, "twitch token vs youtube — must deny (already in Forge table, pinning)"},

		// A hypothetical kick-scoped token matches kick but not twitch.
		{"__inputs.platform.kick.*", "__inputs.platform.kick.zabchannel.last_chat", true, "kick token vs kick — must allow"},
		{"__inputs.platform.kick.*", "__inputs.platform.twitch.zabchannel.last_chat", false, "kick token vs twitch — must deny"},
		{"__inputs.platform.kick.*", "__inputs.platform.youtube.zabchannel.last_chat", false, "kick token vs youtube — must deny"},
	}
	for _, c := range cases {
		if got := matchPath(c.pattern, c.path); got != c.want {
			t.Errorf("[%s] matchPath(%q,%q) = %v, want %v", c.label, c.pattern, c.path, got, c.want)
		}
	}
}
