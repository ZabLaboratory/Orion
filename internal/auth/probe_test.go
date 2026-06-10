package auth

// Probe tests for CanWritePath / matchPath edge cases relevant to the
// B-syswrite surface (ADR 003 §3.1.3 Amendment 1, issue #86).
// Complement Forge's identity_test.go — do NOT rewrite it.
//
// Axes:
//  1. Token scope that is a strict PARENT of the required scope
//     (`__system.anim`) — matchPath remains hierarchical BY DESIGN (it is
//     shared auth code for service-token path scoping, e.g. adapters
//     inbox). The completion endpoint's gate-1 does NOT rely on it any
//     more: it enforces the EXACT scope via set membership (see
//     api/exec_completion.go hasExactScope and the API-level probe
//     tests). The parent-scope behaviour of matchPath is documented
//     here, not treated as a defect; its permissiveness for OTHER
//     scopes is flagged to Bastion.
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

// TestMatchPath_AnimScope_ParentScope_DEFECT: documents that matchPath
// is hierarchical — a parent scope (`__system.anim`) matches the child
// path `__system.anim.report` via `strings.HasPrefix(path, pattern+".")`.
//
// This is matchPath's intended semantics for general service-token path
// scoping and is deliberately NOT changed (shared auth code). The
// completion endpoint's gate-1 (#86, contract §2.3) instead enforces the
// EXACT `__system.anim.report` scope by set membership in
// api/exec_completion.go (hasExactScope), bypassing this hierarchy.
// The rejection of parent scopes at the endpoint is asserted in
// api/exec_completion_probe_test.go.
//
// NOTE for Bastion: matchPath's parent-prefix permissiveness on OTHER
// scopes (e.g. `__inputs.platform`) remains to be evaluated.
func TestMatchPath_AnimScope_ParentScope_DEFECT(t *testing.T) {
	if !matchPath("__system.anim", "__system.anim.report") {
		t.Fatalf("matchPath(%q, %q) = false; matchPath is expected to stay hierarchical — if this changed, re-audit all service-token path scoping", "__system.anim", "__system.anim.report")
	}
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

	// Parent scope: CanWritePath remains hierarchical (true here) BY
	// DESIGN; the completion endpoint does not use it for gate-1 — it
	// requires the exact scope (see api/exec_completion.go and
	// TestMatchPath_AnimScope_ParentScope_DEFECT above).
	parentID := Identity{UserID: "renderer", Role: RoleService, Paths: []string{"__system.anim"}}
	if !parentID.CanWritePath(scope) {
		t.Fatalf("CanWritePath parent-scope hierarchy unexpectedly changed — re-audit service-token scoping")
	}
}
