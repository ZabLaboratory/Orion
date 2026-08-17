package lsdp

import (
	"encoding/json"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// m3AuthoringGraph is the pre-lowering (authoring vocab) equivalent of
// m3Bundle() (bound_leaves_test.go): the same two bindings, in the shape
// EmitLSML actually consumes (LayoutNode.Bindings/Children, the fields it
// documents reading verbatim onto the LSML wire). Used to prove
// boundLeavesFromLSML recovers the identical bound-leaf surface from the
// REAL artefact the stateless path holds (LSML bytes), establishing that
// activating the gate there refuses exactly what the legacy gate already
// refuses on the SAME scene today — nothing new.
func m3AuthoringGraph() compiler.LayoutNode {
	return compiler.LayoutNode{
		Kind: "stack",
		Children: []compiler.LayoutNode{
			{Kind: "text", ID: "board", Bindings: map[string]string{"value": "__vars..leaderboard_display"}},
			{Kind: "text", ID: "chat", Bindings: map[string]string{"value": "chat.display"}},
		},
	}
}

// emitM3LSML runs the real compiler emitter (EmitLSML) over
// m3AuthoringGraph and returns the canonical LSML bytes — the exact
// artefact ZabCanvas's zabcanvas.resolved-scene.v1 envelope carries and
// bluehost.Host.SetBundle stores, per #396's finding.
func emitM3LSML(t *testing.T) []byte {
	t.Helper()
	bundle, _, _, err := compiler.EmitLSML("scene-1", m3AuthoringGraph(), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("EmitLSML: %v", err)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal lsml bundle: %v", err)
	}
	return raw
}

// TestBoundLeavesFromLSML_ParityWithCompiledBundle is the impact-analysis
// proof the #396 "point dur" demands: the bound-leaf surface recovered
// from the REAL stateless-path artefact (LSML bytes) is IDENTICAL to the
// surface the already-proven legacy gate (boundLeavesFromBundle on the
// lowered compiler.RenderBundle) computes for the same authored scene.
// Activating the gate on the stateless path therefore refuses exactly
// what it already refuses today on the legacy path — nothing more.
func TestBoundLeavesFromLSML_ParityWithCompiledBundle(t *testing.T) {
	legacy := boundLeavesFromBundle(m3Bundle())
	fromLSML := boundLeavesFromLSML(emitM3LSML(t))

	if !fromLSML.active() {
		t.Fatal("LSML-derived bound set must be active — the scene has real bindings")
	}
	for _, p := range m3WantKept {
		if !fromLSML.renderable(p) {
			t.Errorf("LSML-derived set must keep %q, matching the legacy gate", p)
		}
		if legacy.renderable(p) != fromLSML.renderable(p) {
			t.Errorf("parity break on %q: legacy=%v lsml=%v", p, legacy.renderable(p), fromLSML.renderable(p))
		}
	}
	for _, p := range m3WantDropped {
		if fromLSML.renderable(p) {
			t.Errorf("LSML-derived set must drop unbound leaf %q", p)
		}
	}
}

// TestMirrorForLSML_GatesUnboundLeaves is TestBoundLeaves_SnapshotEmitsOnlyBound
// (bound_leaves_test.go) driven through the stateless-path entry point,
// MirrorForLSML, on the REAL LSML bytes EmitLSML produces — proving
// boundLeafSet actually executes end to end on this path (#396 resolution
// criterion #1), not merely that the extractor function returns the right
// set in isolation.
func TestMirrorForLSML_GatesUnboundLeaves(t *testing.T) {
	wire, err := NewWire(quietLogger(t), nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorForLSML("scene-1", "", emitM3LSML(t)).(*sceneMirror)

	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State:        m3SceneStateWithInternals(),
	})

	got := wire.kitState("scene-1")
	for _, p := range m3WantKept {
		if _, ok := got[p]; !ok {
			t.Errorf("bound leaf %q was dropped on the stateless (LSML) path", p)
		}
	}
	for _, p := range m3WantDropped {
		if v, ok := got[p]; ok {
			t.Errorf("unbound intermediate %q leaked to the wire as %q — gate inert", p, v)
		}
	}
}

// TestMirrorForLSML_NilBundleDisablesGate is the mutation-proof pin for
// #396: this is EXACTLY the pre-fix behaviour (cmd/orion/main.go hardcoded
// a nil bundle) — reproduced here so the parity is explicit. If a future
// change silently reverts main.go's wiring back to a hardcoded nil, this
// test alone cannot catch it (it exercises MirrorForLSML directly), but
// TestStartBridge_ThreadsHostBundleIntoMirrorFor (internal/api) and
// TestMirrorForLSML_GatesUnboundLeaves together do: the former proves real
// bytes reach the closure, the latter proves those bytes gate leaves —
// reverting either fix fails one of the two.
func TestMirrorForLSML_NilBundleDisablesGate(t *testing.T) {
	if boundLeavesFromLSML(nil).active() {
		t.Fatal("a nil LSML bundle must yield a disabled (inactive) gate — fail-open, never a black screen")
	}
	wire, err := NewWire(quietLogger(t), nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	m := wire.MirrorForLSML("scene-1", "", nil).(*sceneMirror)
	m.Forward(&protocol.Snapshot{
		SceneID:      "scene-1",
		SceneVersion: "sha256:test-1",
		State: map[string]json.RawMessage{
			"catA0": json.RawMessage(`"x"`),
		},
	})
	got := wire.kitState("scene-1")
	if _, ok := got["catA0"]; !ok {
		t.Errorf("gate disabled (nil bundle): scalar leaf catA0 must still pass the scalar filter")
	}
}
