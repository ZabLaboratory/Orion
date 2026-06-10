package adapters

// Probe tests — B-syswrite hardening (ADR 003 §3.1.3 / wire contract §2.6,
// issue #85). Complement Forge's inbox_system_test.go — never rewrite it.
//
// Axes:
//   1. Bare `__system` path (no dot suffix) is refused wire-write for all roles
//   2. Any path with `__system.` prefix of arbitrary depth is refused
//   3. system mark is NOT exported — struct literal from any external
//      package produces system=false (compile-level, distinct from Forge's test
//      which is same-package; this one uses go vet / language rules)
//   4. systemWrite helper (in-package) does set the flag — ensures the
//      internal path works (tick fan-out) even as external path is blocked
//   5. Operator and admin identities cannot fan-out via __system.* even
//      with a broad Paths grant
//   6. Wire write of `__system` bare (no prefix `__system.`) is also refused

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/auth"
)

// TestInbox_BareSystemPathRefused: the exact string "__system" (no trailing
// dot) must also be refused from the wire — isSystemNamespace covers both
// "== __system" and "HasPrefix __system.".
func TestInbox_BareSystemPathRefused(t *testing.T) {
	_, inbox := systemTestShow(t)
	identities := []auth.Identity{
		{UserID: "op", Role: auth.RoleOperator},
		{UserID: "adm", Role: auth.RoleAdmin},
		{UserID: "svc", Role: auth.RoleService, Paths: []string{"__system"}},
	}
	for _, id := range identities {
		err := inbox.Write(context.Background(), Write{
			Identity: id,
			Path:     "__system",
			Value:    json.RawMessage(`{}`),
			Source:   "probe",
		})
		if !errors.Is(err, ErrWriteForbidden) {
			t.Fatalf("role %s: bare __system write must be forbidden, got %v", id.Role, err)
		}
	}
}

// TestInbox_DeepSystemNamespaceRefused: arbitrary-depth paths under
// __system.* (e.g. __system.anim.report.foo.bar) are refused from the wire
// for all identities regardless of their Paths grant.
func TestInbox_DeepSystemNamespaceRefused(t *testing.T) {
	_, inbox := systemTestShow(t)
	deepPaths := []string{
		"__system.anim.report",
		"__system.anim.report.foo",
		"__system.tick",
		"__system.x.y.z.w",
		"__system.", // trailing dot — still HasPrefix
	}
	svc := auth.Identity{
		UserID: "svc",
		Role:   auth.RoleService,
		Paths:  []string{"__system.*", "__system.anim.report", "__system."},
	}
	for _, p := range deepPaths {
		err := inbox.Write(context.Background(), Write{
			Identity: svc,
			Path:     p,
			Value:    json.RawMessage(`{}`),
			Source:   "probe",
		})
		if !errors.Is(err, ErrWriteForbidden) {
			t.Fatalf("path %q with service token: must be forbidden, got %v", p, err)
		}
	}
}

// TestInbox_SystemWriteHelperSetsFlag: the in-package systemWrite function
// sets the unexported system field; the resulting Write bypasses the scope
// check and fans out to the scene (tick path). This test confirms the
// internal path still works after any refactor of the flag.
func TestInbox_SystemWriteHelperSetsFlag(t *testing.T) {
	w := systemWrite(Write{
		Path:   "__system.tick",
		Value:  json.RawMessage(`999`),
		Source: "system:tick",
	})
	if !w.system {
		t.Fatal("systemWrite helper must set the system flag to true")
	}
}

// TestInbox_OperatorCannotFanOutSystemViaGrantedPath: an operator identity
// given a `Paths` that includes `__system.*` (simulating an overly broad
// JWT) is still refused at the __system.* gate — the namespace check runs
// BEFORE the path-match check, so no grant can override it.
func TestInbox_OperatorCannotFanOutSystemViaGrantedPath(t *testing.T) {
	_, inbox := systemTestShow(t)
	// Operator with an overly broad Paths grant (should never happen in
	// prod, but the gate must hold even if it does).
	op := auth.Identity{
		UserID: "op",
		Role:   auth.RoleOperator,
		Paths:  []string{"__system.*", "__system.anim.report"},
	}
	err := inbox.Write(context.Background(), Write{
		Identity: op,
		Path:     "__system.anim.report",
		Value:    json.RawMessage(`{"wake_key":"wk|sha256:test|0|1"}`),
		Source:   "probe",
	})
	if !errors.Is(err, ErrWriteForbidden) {
		t.Fatalf("operator with __system.* grant must still be refused at namespace gate, got %v", err)
	}
}

// TestInbox_SystemFanOut_DoesNotReachSceneForExternalWrite: confirms the
// double-gate defence in depth — even if sceneAcceptsPath were somehow
// called with system=false for a __system.* path, it must return false
// (the fan-out shortcut requires system=true). Covers the defense-in-depth
// comment in inbox.go L197-203.
func TestInbox_SystemFanOut_DoesNotReachSceneForExternalWrite(t *testing.T) {
	show, _ := systemTestShow(t)
	scene, err := show.Get("s1")
	if err != nil {
		t.Fatal(err)
	}

	// With system=false the __system.* fan-out shortcut must not trigger.
	if sceneAcceptsPath(scene, "__system.tick", false) {
		t.Fatal("sceneAcceptsPath must return false for __system.* with system=false (defense-in-depth)")
	}
	if sceneAcceptsPath(scene, "__system.anim.report", false) {
		t.Fatal("sceneAcceptsPath must return false for __system.anim.report with system=false")
	}
	// With system=true the fan-out shortcut fires (tick path).
	if !sceneAcceptsPath(scene, "__system.tick", true) {
		t.Fatal("sceneAcceptsPath must return true for __system.tick with system=true (tick fan-out)")
	}
}
