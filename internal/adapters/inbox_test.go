package adapters

import (
	"encoding/json"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// quietLogger is shared with poller_test.go in this package.

// sceneWithOperatorInput builds a scene whose compiled graph declares a
// single operator_input leaf (path A with a seeded default) — the M9 shape
// the fixed compiler emits: the path is in BOTH graph.Defaults (seeded)
// and graph.OperatorInputs (the accept set).
func sceneWithOperatorInput(t *testing.T, leaf string) *runtime.Scene {
	t.Helper()
	graph := &compiler.Graph{
		SceneID:      "scene-op",
		SceneVersion: "sha256:test",
		Defaults: map[string]json.RawMessage{
			leaf: json.RawMessage(`"A"`),
		},
		OperatorInputs: []compiler.OperatorInput{
			{Path: leaf, Label: "Headline", Type: "text", Default: json.RawMessage(`"A"`)},
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}
	return runtime.NewScene("scene-op", graph, bundle, runtime.NewComputeRegistry(), quietLogger())
}

// TestSceneAcceptsPath_OperatorInput is the M9/D2 write-accept half: a
// write to a declared operator_input leaf must be accepted (so the
// operator's push of B reaches the scene and repaints), NOT silently
// dropped. sceneAcceptsPath consults graph.OperatorInputs (and Defaults);
// both now carry the leaf, so the write is authorised.
func TestSceneAcceptsPath_OperatorInput(t *testing.T) {
	scene := sceneWithOperatorInput(t, "headline.text")

	if !sceneAcceptsPath(scene, "headline.text", false) {
		t.Fatal("write to declared operator_input leaf rejected — push of B would be dropped (delivered=false)")
	}
	// An undeclared leaf is still refused (no over-broad accept).
	if sceneAcceptsPath(scene, "not.declared", false) {
		t.Fatal("undeclared leaf accepted — accept set is too broad")
	}
}

// TestSceneAcceptsPath_StaticSceneUnchanged guards M8: a scene with no
// operator_inputs and no matching default still refuses an arbitrary
// write (no behavioural drift from the operator-input plumbing).
func TestSceneAcceptsPath_StaticSceneUnchanged(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "scene-static",
		SceneVersion: "sha256:test",
		Defaults:     map[string]json.RawMessage{"score.team_a": json.RawMessage(`0`)},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}
	scene := runtime.NewScene("scene-static", graph, bundle, runtime.NewComputeRegistry(), quietLogger())

	// A leaf seeded in Defaults is accepted (pre-existing rule, unchanged).
	if !sceneAcceptsPath(scene, "score.team_a", false) {
		t.Fatal("a default-seeded leaf must stay acceptable (M8 regression)")
	}
	// An undeclared leaf is refused.
	if sceneAcceptsPath(scene, "headline.text", false) {
		t.Fatal("static scene must not accept an undeclared operator_input leaf")
	}
}
