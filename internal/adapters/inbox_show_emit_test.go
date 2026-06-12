package adapters

import (
	"encoding/json"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// `show.emit` active-only injection at the REAL inbox seam (ADR 009 §3.6,
// issue #155). Inbox.EmitToActive is the production runtime.Emitter: it
// delivers a SYSTEM `__events.<topic>` write to show.Active() ONLY — never
// the §3.3 RouteTargets union — so a stream rule's emission can never
// cascade rule→rule (anti-loop by construction), and it records ONE audit
// entry per emission (criterion #4: the event appears in the audit ring).
//
// The executor + on-event firing + no-cascade end-to-end is proven in the
// runtime package (TestShowEmit_RuleToActiveNoCascade); here we prove the
// real injection PATH: which scene the write lands on (active only), and
// the audit trace. We observe delivery via the bespoke per-scene delta (the
// injected __events leaf is dirty → patched), independent of any on-event
// program.

const emitTopic = "alert"
const emitLeaf = "__events." + emitTopic

// TestEmitToActive_ActiveOnlyNoCascade: Inbox.EmitToActive delivers the
// `__events.<topic>` write to the active scene ONLY. A promoted rule —
// which IS in the RouteTargets union for wire writes — receives NOTHING
// from an emit, because emit deliberately bypasses RouteTargets. This is
// the anti-loop invariant at the real seam.
func TestEmitToActive_ActiveOnlyNoCascade(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}

	show.Load("scene-active", platformGraph("scene-active"), bundle)
	if err := show.SetActive("scene-active", nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if err := show.PromoteStreamRule("rule-1", platformGraph("rule-1"), bundle); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}

	active, _ := show.Get("scene-active")
	rule, _ := show.Get("rule-1")
	activeSub, _ := active.Subscribe(16)
	ruleSub, _ := rule.Subscribe(16)

	inbox := NewInbox(show, quietLogger(), nil)
	show.SetEmitter(inbox)

	// Sanity: this rule IS in the union for a WIRE write (it would receive a
	// platform write). The emit below must still skip it — proving emit is a
	// distinct path, not the union.
	if got := len(show.RouteTargets()); got != 2 {
		t.Fatalf("RouteTargets size = %d, want 2 (active + rule)", got)
	}

	inbox.EmitToActive(emitTopic, json.RawMessage(`{"k":"v"}`))

	// The active scene received the injected event leaf.
	awaitLeafDelta(t, activeSub, emitLeaf)
	// The promoted rule did NOT — emit is active-only, never rule→rule.
	expectNoLeafDelta(t, ruleSub, emitLeaf)

	// Exactly one audit entry, for the emission, on the right path.
	snap := inbox.audit.Snapshot()
	var emitEntries int
	for _, e := range snap {
		if e.Path == emitLeaf {
			emitEntries++
			if e.Source != "system:show.emit" {
				t.Fatalf("emit audit source = %q, want system:show.emit", e.Source)
			}
		}
	}
	if emitEntries != 1 {
		t.Fatalf("audit ring has %d emit entries, want 1 (the single emission)", emitEntries)
	}
}

// TestEmitToActive_NoActiveSceneAbsorbed: with no active scene, EmitToActive
// is absorbed (audited, delivered nowhere) — exactly as a wire write with no
// active scene. A promoted rule still receives nothing (no cascade fallback).
func TestEmitToActive_NoActiveSceneAbsorbed(t *testing.T) {
	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}

	if err := show.PromoteStreamRule("rule-1", platformGraph("rule-1"), bundle); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}
	rule, _ := show.Get("rule-1")
	ruleSub, _ := rule.Subscribe(16)

	inbox := NewInbox(show, quietLogger(), nil)
	show.SetEmitter(inbox)

	inbox.EmitToActive(emitTopic, json.RawMessage(`{"k":"v"}`))

	// No active scene → the rule never receives the emit (no fallback to the
	// union). The emission is still audited.
	expectNoLeafDelta(t, ruleSub, emitLeaf)
	snap := inbox.audit.Snapshot()
	found := false
	for _, e := range snap {
		if e.Path == emitLeaf {
			found = true
		}
	}
	if !found {
		t.Fatal("emit with no active scene must still be audited (absorbed, not silent)")
	}
}
