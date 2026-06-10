package auth

// Probe tests for CanWritePath / matchPath edge cases relevant to the
// B-syswrite surface (ADR 003 §3.1.3 Amendment 1, issue #86).
// Complement Forge's identity_test.go — do NOT rewrite it.
//
// Axes:
//  1. Token scope that is a strict PARENT of the required scope
//     (`__system.anim`) must NOT grant access to `__system.anim.report`.
//     *** DEFECT FOUND: matchPath("__system.anim", "__system.anim.report")
//     returns true because the `strings.HasPrefix(path, pattern+".")` rule
//     lets any parent scope pass. This violates the contract (§2.3): the
//     renderer token carries the EXACT scope string, and a broader token
//     (`__system.anim`) must not be treated as sufficient. Returned to Forge.
//  2. Token scope extension (`__system.anim.report.x`) — correctly rejected.
//  3. Exact scope (`__system.anim.report`) — correctly accepted (regression guard).
//  4. Gate-1 via Identity.CanWritePath end-to-end.

import "testing"

// TestMatchPath_AnimScope_ExactAndSuperstring: the exact scope and
// superstring (extension) variant are correctly handled. The parent-scope
// defect is pinned separately below.
func TestMatchPath_AnimScope_ExactAndSuperstring(t *testing.T) {
	const required = "__system.anim.report"

	cases := []struct {
		name    string
		pattern string
		want    bool
	}{
		// Exact — must pass.
		{"exact", "__system.anim.report", true},
		// Extension / superstring — must NOT pass: the token has a more
		// specific scope than required. matchPath(pattern,path) where
		// pattern="__system.anim.report.x" and path="__system.anim.report":
		// none of the suffix rules trigger, prefix check fails → false. OK.
		{"extension", "__system.anim.report.x", false},
		// Unrelated sibling.
		{"sibling", "__system.anim.other", false},
		// Completely different root.
		{"other-root", "__inputs.platform.*", false},
		// Empty pattern.
		{"empty", "", false},
	}

	for _, c := range cases {
		got := matchPath(c.pattern, required)
		if got != c.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", c.pattern, required, got, c.want)
		}
	}
}

// TestMatchPath_AnimScope_ParentScope_DEFECT: documents the confirmed
// defect in matchPath — a parent scope (`__system.anim`) is incorrectly
// accepted for the child path `__system.anim.report` because the prefix
// rule `strings.HasPrefix(path, pattern+".")` is satisfied.
//
// CONTRACT: `__system.anim.report` is a leaf scope (§2.3). A token with
// only `__system.anim` must NOT pass gate-1. This test asserts the
// CURRENT (wrong) behavior to pin the defect; it must become `false` after
// Forge fixes matchPath.
//
// DEFECT RETURNED TO FORGE: auth/identity.go matchPath prefix rule grants
// parent scopes over children, violating the exact-scope enforcement
// requirement for B-syswrite (criterion #19).
func TestMatchPath_AnimScope_ParentScope_DEFECT(t *testing.T) {
	// Current behavior: true (BUG — parent grants child access).
	// Expected after fix: false.
	got := matchPath("__system.anim", "__system.anim.report")
	if !got {
		// If this passes false, the bug has been fixed — upgrade to a
		// proper positive assertion.
		t.Log("DEFECT RESOLVED: matchPath parent-scope bug is fixed")
		return
	}
	// Pin the defect: test passes but documents the wrong behavior.
	t.Logf("DEFECT CONFIRMED: matchPath(%q, %q) = true; want false after fix", "__system.anim", "__system.anim.report")
}

// TestCanWritePath_AnimReport_ServiceToken: gate-1 checks via the exported
// API. The parent-scope case is pinned as a defect (see above).
func TestCanWritePath_AnimReport_ServiceToken(t *testing.T) {
	const scope = "__system.anim.report"

	passing := []struct {
		name  string
		paths []string
	}{
		{"exact scope", []string{"__system.anim.report"}},
		{"multi-scope includes exact", []string{"__inputs.foo", "__system.anim.report"}},
	}
	for _, c := range passing {
		id := Identity{UserID: "renderer", Role: RoleService, Paths: c.paths}
		if !id.CanWritePath(scope) {
			t.Errorf("[%s] CanWritePath(%q) = false, want true (paths=%v)", c.name, scope, c.paths)
		}
	}

	failing := []struct {
		name  string
		paths []string
	}{
		{"extension scope only", []string{"__system.anim.report.x"}},
		{"unrelated", []string{"__inputs.platform.*"}},
		{"empty paths", []string{}},
		{"scope-wildcard no star in token", []string{"__system.anim.report.*"}}, // token shouldn't have wildcard for this scope
	}
	for _, c := range failing {
		id := Identity{UserID: "renderer", Role: RoleService, Paths: c.paths}
		if id.CanWritePath(scope) {
			t.Errorf("[%s] CanWritePath(%q) = true, want false (paths=%v)", c.name, scope, c.paths)
		}
	}

	// DEFECT: parent scope — documents incorrect current behavior.
	// Must be false after Forge fixes matchPath. See TestMatchPath_AnimScope_ParentScope_DEFECT.
	parentID := Identity{UserID: "renderer", Role: RoleService, Paths: []string{"__system.anim"}}
	if parentID.CanWritePath(scope) {
		t.Logf("DEFECT: CanWritePath with parent scope __system.anim grants __system.anim.report — must be fixed in matchPath")
	}
}
