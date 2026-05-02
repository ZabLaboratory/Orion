package auth

import (
	"net/http"
	"testing"
)

func TestFromHeaders_AnonymousWhenMissing(t *testing.T) {
	id := FromHeaders(http.Header{})
	if id.IsAuthenticated() {
		t.Fatalf("expected anonymous, got %+v", id)
	}
}

func TestFromHeaders_OperatorTrust(t *testing.T) {
	h := http.Header{}
	h.Set("X-Authenticated-User", "user-42")
	h.Set("X-Authenticated-Role", "operator")
	id := FromHeaders(h)
	if !id.IsAuthenticated() {
		t.Fatal("operator should be authenticated")
	}
	if id.Role != RoleOperator {
		t.Fatalf("role=%q", id.Role)
	}
	if !id.CanWritePath("score.team_a") {
		t.Fatal("operator must write any path the runtime declares writable")
	}
}

func TestFromHeaders_ServicePathScope(t *testing.T) {
	h := http.Header{}
	h.Set("X-Authenticated-User", "quasar")
	h.Set("X-Authenticated-Role", "service")
	h.Set("X-Authenticated-Paths", "__inputs.platform.*, __inputs.scheduler.*")
	id := FromHeaders(h)

	cases := []struct {
		path  string
		write bool
	}{
		{"__inputs.platform.twitch.zab.last_chat", true},
		{"__inputs.platform.youtube.foo.last_chat", true},
		{"__inputs.scheduler.cue.next", true},
		{"score.team_a", false},
		{"__inputs.unrelated.field", false},
	}
	for _, c := range cases {
		if got := id.CanWritePath(c.path); got != c.write {
			t.Errorf("CanWritePath(%q) = %v, want %v", c.path, got, c.write)
		}
	}
}

func TestFromHeaders_ViewerNeverWrites(t *testing.T) {
	h := http.Header{}
	h.Set("X-Authenticated-User", "pulsar-cef-7")
	h.Set("X-Authenticated-Role", "viewer")
	id := FromHeaders(h)
	if id.CanWritePath("anything") {
		t.Fatal("viewer must not write")
	}
}

func TestMatchPath_ExactAndPrefix(t *testing.T) {
	cases := []struct {
		pattern, path string
		match         bool
	}{
		{"score.team_a", "score.team_a", true},
		{"score.team_a", "score.team_b", false},
		{"score", "score.team_a", true},  // strict prefix match (terminated by `.`)
		{"score", "scoreboard", false},
		{"__inputs.*", "__inputs.foo", true},
		{"__inputs.*", "__inputs.deeply.nested.thing", true},
		{"__inputs.platform.*", "__inputs.platform.twitch.x", true},
		{"__inputs.platform.*", "__inputs.scheduler.x", false},
	}
	for _, c := range cases {
		if got := matchPath(c.pattern, c.path); got != c.match {
			t.Errorf("matchPath(%q,%q)=%v, want %v", c.pattern, c.path, got, c.match)
		}
	}
}
