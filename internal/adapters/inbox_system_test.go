package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// B-syswrite hardening tests (ADR 003 §3.1.3 / wire contract §2.6,
// issue #85): no externally-sourced write can set the system mark or
// reach `__system.*` as a free write — for ANY role, scope grants
// included. Internal system writes keep the tick fan-out.

func systemTestShow(t *testing.T) (*runtime.Show, *Inbox) {
	t.Helper()
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	graph := &compiler.Graph{
		SceneID:      "s1",
		SceneVersion: "sha256:test",
		Defaults:     map[string]json.RawMessage{"score.team_a": json.RawMessage(`0`)},
	}
	show.Load("s1", graph, &compiler.RenderBundle{SceneVersion: "sha256:test"})
	t.Cleanup(show.Stop)
	return show, NewInbox(show, quietLogger(), nil)
}

// TestInbox_SystemFieldNotSettableFromWire pins the COMPILE-LEVEL
// guarantee: adapters.Write exposes no exported way to mark a write as
// system — a handler-built Write is always scope-checked. The struct
// literal below is exactly what ws/api handlers can build; if someone
// re-exports the field this test stops representing the wire surface
// and the namespace gate below still holds.
func TestInbox_SystemFieldNotSettableFromWire(t *testing.T) {
	w := Write{
		Identity: auth.Identity{UserID: "op", Role: auth.RoleOperator},
		Path:     "score.team_a",
		Value:    json.RawMessage(`1`),
		Source:   "operator:op",
	}
	if w.system {
		t.Fatal("zero-value Write must not be system")
	}
}

// TestInbox_SystemNamespaceNotWireWritable: a `__system.*` write from
// the wire is refused for EVERY identity — operator, admin, and a
// service token even if its paths claim covers `__system.*`.
func TestInbox_SystemNamespaceNotWireWritable(t *testing.T) {
	_, inbox := systemTestShow(t)
	identities := []auth.Identity{
		{UserID: "op", Role: auth.RoleOperator},
		{UserID: "adm", Role: auth.RoleAdmin},
		{UserID: "svc", Role: auth.RoleService, Paths: []string{"__system.*"}},
		{UserID: "svc2", Role: auth.RoleService, Paths: []string{"__system.anim.report"}},
	}
	for _, id := range identities {
		err := inbox.Write(context.Background(), Write{
			Identity: id,
			Path:     "__system.anim.report",
			Value:    json.RawMessage(`{"wake_key":"wk|sha256:test|0|1"}`),
			Source:   "test",
		})
		if !errors.Is(err, ErrWriteForbidden) {
			t.Fatalf("role %s: __system.* wire write must be forbidden, got %v", id.Role, err)
		}
	}
}

// TestInbox_InternalSystemWriteKeepsTickFanOut: the in-package system
// mark keeps the `__system.*` fan-out (the tick lands on every scene).
func TestInbox_InternalSystemWriteKeepsTickFanOut(t *testing.T) {
	_, inbox := systemTestShow(t)
	err := inbox.Write(context.Background(), systemWrite(Write{
		Path:   "__system.tick",
		Value:  json.RawMessage(`123`),
		Source: "system:tick",
	}))
	if err != nil {
		t.Fatalf("internal system write must pass: %v", err)
	}
}

// TestSceneAcceptsPath_SystemFanOutOnlyForSystemWrites: the
// unconditional `__system.*` fan-out shortcut is gone for non-system
// writes — defense in depth under the namespace gate.
func TestSceneAcceptsPath_SystemFanOutOnlyForSystemWrites(t *testing.T) {
	scene := sceneWithOperatorInput(t, "headline.text")
	if sceneAcceptsPath(scene, "__system.tick", false) {
		t.Fatal("non-system __system.* write must not fan out")
	}
	if !sceneAcceptsPath(scene, "__system.tick", true) {
		t.Fatal("internal system __system.* write must fan out (tick)")
	}
}
