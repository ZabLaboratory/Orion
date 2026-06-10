package api

// Probe tests for the external completion endpoint (B-syswrite, issue #86).
// Complement Forge's exec_completion_test.go — do NOT rewrite it.
//
// Axes:
//  1. Scope substring gate-1 hardening: a token whose paths contain a
//     PARENT scope (`__system.anim`) must NOT pass gate-1. Originally a
//     DEFECT (CanWritePath's parent-prefix rule); fixed by Forge: gate-1
//     now requires the EXACT scope via set membership (hasExactScope),
//     not the hierarchical matchPath. These tests assert the rejection.
//  2. Double-report idempotency: the SAME wake key sent twice. First
//     report resumes the continuation; second is an unknown drop
//     (counted, resumes nothing). The body is byte-identical on both.
//  3. No-leak byte-identity across ALL drop reasons: role, scene,
//     malformed, kind, unknown — each response body must equal the
//     accepted body.
//  4. Concurrent endpoint hammering + scene loop under -race: multiple
//     goroutines POST the same (correct) wake key simultaneously. Only
//     one resumes; the rest are counted as unknown drops. No data race.

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// --- 1. Scope parent / substring gate-1 hardening -------------------------

// TestCompletion_Gate1_ScopeParentIsDefect: a service token carrying
// `__system.anim` (parent of the required `__system.anim.report`) must be
// REJECTED at gate-1 (contract §2.3 requires the exact scope). Originally
// pinned a defect (CanWritePath's parent-prefix rule let parents through);
// gate-1 now uses exact set membership, so this asserts the fix: 202 drop,
// reason=role, continuation never resumed. Wildcard and root parents
// (`__system.*`, `__system`) are covered as variants.
func TestCompletion_Gate1_ScopeParentIsDefect(t *testing.T) {
	for _, scope := range []string{"__system.anim", "__system.*", "__system"} {
		t.Run(scope, func(t *testing.T) {
			f := newAnimFixture(t, "scope-parent-"+scope)

			parentScopeHeaders := map[string]string{
				"X-Authenticated-User":  "bad-renderer",
				"X-Authenticated-Role":  "service",
				"X-Authenticated-Paths": scope, // parent/wildcard — must not pass
			}
			w := postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), parentScopeHeaders)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", w.Code)
			}

			roleDrops := testutil.ToFloat64(f.metrics.ComplRejected.WithLabelValues(f.sceneID, "role"))
			if roleDrops != 1 {
				t.Fatalf("parent scope %q must be rejected at gate-1: role drops = %v, want 1", scope, roleDrops)
			}
			notResumed(t, f)
		})
	}
}

// TestCompletion_Gate1_ScopeExtension_Rejected: a service token whose
// paths contain a SUPERSTRING scope (`__system.anim.report.x`) must be
// rejected at gate-1. Unlike the parent-scope defect, the extension
// correctly fails matchPath (the requested path is not prefixed by the
// extended pattern). Regression guard.
func TestCompletion_Gate1_ScopeExtension_Rejected(t *testing.T) {
	f := newAnimFixture(t, "scope-extension")

	extHeaders := map[string]string{
		"X-Authenticated-User":  "renderer",
		"X-Authenticated-Role":  "service",
		"X-Authenticated-Paths": "__system.anim.report.x",
	}
	w := postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), extHeaders)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	roleDrops := testutil.ToFloat64(f.metrics.ComplRejected.WithLabelValues(f.sceneID, "role"))
	if roleDrops != 1 {
		t.Fatalf("extension scope should be rejected at gate-1: role drops = %v, want 1", roleDrops)
	}
	notResumed(t, f)
}

// TestCompletion_Gate1_ScopeSubstring_Rejected: `__system.anim` is a
// SUBSTRING of the required scope — must not pass gate-1 (exact-match
// enforcement). Cross-checks the parent-scope rejection via a slightly
// different angle (single-scope header, no other scopes in the list).
// Also verifies the exact scope still passes when mixed into a CSV.
func TestCompletion_Gate1_ScopeSubstring_Rejected(t *testing.T) {
	f := newAnimFixture(t, "scope-substring")
	headers := map[string]string{
		"X-Authenticated-User":  "renderer",
		"X-Authenticated-Role":  "service",
		"X-Authenticated-Paths": "__system.anim",
	}
	w := postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), headers)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	roleDrops := testutil.ToFloat64(f.metrics.ComplRejected.WithLabelValues(f.sceneID, "role"))
	if roleDrops != 1 {
		t.Fatalf("substring scope __system.anim must be rejected at gate-1: role drops = %v, want 1", roleDrops)
	}
	notResumed(t, f)

	// Positive control: exact scope among other scopes in the CSV passes.
	f2 := newAnimFixture(t, "scope-csv-exact")
	csvHeaders := map[string]string{
		"X-Authenticated-User":  "renderer",
		"X-Authenticated-Role":  "service",
		"X-Authenticated-Paths": "__inputs.foo.*, __system.anim.report, __system.anim",
	}
	w2 := postCompletion(t, f2, f2.sceneID, reportBody(f2.wakeKey), csvHeaders)
	if w2.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w2.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f2.doneLeaf(t) != `"done"` {
		if !time.Now().Before(deadline) {
			t.Fatalf("exact scope in CSV: continuation never resumed")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- 2. Double-report idempotency ----------------------------------------

// TestCompletion_DoubleReport_SecondIsUnknownDrop: sending the SAME wake
// key twice. The first report resumes the continuation (sets done="done").
// The second is an unknown drop — counted, does not crash, body is
// byte-identical (no-leak).
func TestCompletion_DoubleReport_SecondIsUnknownDrop(t *testing.T) {
	f := newAnimFixture(t, "scene-double-report")

	// First report — resumes.
	w1 := postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), rendererHeaders())
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first report: status = %d, want 202", w1.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.doneLeaf(t) != `"done"` {
		if !time.Now().Before(deadline) {
			t.Fatalf("first report: continuation never resumed")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Second report — same key, continuation already consumed → unknown drop.
	w2 := postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), rendererHeaders())
	if w2.Code != http.StatusAccepted {
		t.Fatalf("second report: status = %d, want 202", w2.Code)
	}
	waitCounter(t, "second report unknown drop", func() float64 {
		return testutil.ToFloat64(f.metrics.ComplRejected.WithLabelValues(f.sceneID, "unknown"))
	}, 1)

	// Body byte-identical (no-leak).
	if w1.Body.String() != w2.Body.String() {
		t.Fatalf("double-report: body differs between accepted and unknown drop — leaks continuation state")
	}

	// Continuation still in final state, not re-run.
	if v := f.doneLeaf(t); v != `"done"` {
		t.Fatalf("double-report: done leaf changed after second report: %s", v)
	}
}

// --- 3. No-leak: ALL drop reasons produce byte-identical body -------------

// TestCompletion_NoLeak_AllReasons_BodyIdentical: each drop reason
// (role, scene, malformed, kind, unknown) must produce the exact same
// response body as a legitimately accepted report. Any divergence lets a
// caller probe the system for continuation existence.
//
// This test uses a FRESH fixture per reason to avoid wake-key consumption
// ordering issues.
func TestCompletion_NoLeak_AllReasons_BodyIdentical(t *testing.T) {
	// Get the reference body from a fresh accepted report.
	ref := newAnimFixture(t, "no-leak-ref")
	refW := postCompletion(t, ref, ref.sceneID, reportBody(ref.wakeKey), rendererHeaders())
	if refW.Code != http.StatusAccepted {
		t.Fatalf("reference accepted report: status = %d", refW.Code)
	}
	refBody := refW.Body.String()

	// Each case uses a FRESH fixture so its wake key is unconsumed.
	fRole := newAnimFixture(t, "no-leak-role")
	fScene := newAnimFixture(t, "no-leak-scene")
	fMalformed := newAnimFixture(t, "no-leak-malformed")
	fKind := newAnimFixture(t, "no-leak-kind")
	fUnknown := newAnimFixture(t, "no-leak-unknown")

	cases := []struct {
		name    string
		f       *animFixture
		sceneID string
		body    string
		headers map[string]string
	}{
		{
			"role-drop",
			fRole, fRole.sceneID,
			reportBody(fRole.wakeKey),
			map[string]string{"X-Authenticated-Role": "operator"},
		},
		{
			"scene-drop",
			fScene, "no-such-scene-nl",
			reportBody(fScene.wakeKey),
			rendererHeaders(),
		},
		{
			"malformed-drop",
			fMalformed, fMalformed.sceneID,
			`not-json`,
			rendererHeaders(),
		},
		{
			"kind-drop",
			fKind, fKind.sceneID,
			`{"wake_key":"` + fKind.wakeKey + `","kind":"video"}`,
			rendererHeaders(),
		},
		{
			"unknown-drop",
			fUnknown, fUnknown.sceneID,
			reportBody("wk|sha256:api-anim|0|777"),
			rendererHeaders(),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := completionReq(c.sceneID, c.body, c.headers)
			postExecCompletion(c.f.deps)(w, req)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", w.Code)
			}
			if w.Body.String() != refBody {
				t.Fatalf("body %q != reference accepted body %q — leaks (reason %s)", w.Body.String(), refBody, c.name)
			}
		})
	}
}

// --- 4. Concurrent endpoint + scene loop under -race ----------------------

// TestCompletion_Concurrent_EndpointRace: N goroutines POST the
// completion endpoint with the SAME valid wake key simultaneously while
// the scene loop is running. Exactly one must resume the continuation;
// the rest must land as unknown drops. No data race (single-writer
// invariant: wheel/parked map only touched inside the scene goroutine).
func TestCompletion_Concurrent_EndpointRace(t *testing.T) {
	const goroutines = 20
	f := newAnimFixture(t, "scene-concurrent-race")

	var wg sync.WaitGroup
	wg.Add(goroutines)
	responses := make([]*httptest.ResponseRecorder, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			responses[idx] = postCompletion(t, f, f.sceneID, reportBody(f.wakeKey), rendererHeaders())
		}(i)
	}
	wg.Wait()

	// All responses must be 202.
	for i, w := range responses {
		if w.Code != http.StatusAccepted {
			t.Errorf("goroutine %d: status = %d, want 202", i, w.Code)
		}
	}

	// Continuation must have been resumed exactly once.
	deadline := time.Now().Add(2 * time.Second)
	for f.doneLeaf(t) != `"done"` {
		if !time.Now().Before(deadline) {
			t.Fatalf("concurrent race: continuation never resumed")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// goroutines-1 must be unknown drops (exactly one resume possible).
	waitCounter(t, "unknown drops = goroutines-1", func() float64 {
		return testutil.ToFloat64(f.metrics.ComplRejected.WithLabelValues(f.sceneID, "unknown"))
	}, float64(goroutines-1))
}
