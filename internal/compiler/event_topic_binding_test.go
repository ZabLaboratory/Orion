package compiler

import (
	"context"
	"encoding/json"
	"testing"
)

// onEventBlueprint: on-event(event_name=topic) → variable.set(fired). One
// exec entry whose topic must surface as a synthesized event-topic
// acceptance binding (issue #148, ADR 008 §3.3).
func onEventTopicBlueprint(topic string) *BlueprintGraph {
	return &BlueprintGraph{
		ID: "bp-evt",
		Nodes: []BlueprintNode{
			{ID: "onevt", Compute: "core.event.on-event@1",
				Config:  map[string]json.RawMessage{"event_name": json.RawMessage(`"` + topic + `"`)},
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "val", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`1`)},
				Outputs: []BlueprintPort{dataIn("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"fired"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "onevt", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "val", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}
}

func compileOnEvent(t *testing.T, topic string) (*Graph, string) {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": onEventTopicBlueprint(topic)},
		manifest:   execManifest(),
	}
	g, _, version, err := Compile(context.Background(), "scene-evt",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatalf("on-event scene rejected: %v", err)
	}
	return g, version
}

// TestCompile_OnEvent_SynthesizesEventTopicBinding is issue #148 / ADR 008
// §3.3 criterion #2 (compile half): an on-event entry's topic surfaces as
// an ExternalAdapter{Kind:"event-topic", TargetPaths:["__events.<topic>"]}
// — the mirror of platform-stream — so sceneAcceptsPath routes a write to
// __events.<topic> to the scene.
func TestCompile_OnEvent_SynthesizesEventTopicBinding(t *testing.T) {
	g, _ := compileOnEvent(t, "goal")

	var found *ExternalAdapter
	for i := range g.Bindings {
		if g.Bindings[i].Kind == "event-topic" {
			found = &g.Bindings[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no event-topic binding synthesized; bindings = %+v", g.Bindings)
	}
	if len(found.TargetPaths) != 1 || found.TargetPaths[0] != "__events.goal" {
		t.Fatalf("event-topic target = %v, want [__events.goal]", found.TargetPaths)
	}
	// Pure acceptance: no goroutine-bearing fields set (mirror of
	// platform-stream, ADR 008 §3.3).
	if found.URL != "" || found.FrequencyHz != nil {
		t.Fatalf("event-topic binding carries adapter fields (URL=%q freq=%v) — must be pure acceptance",
			found.URL, found.FrequencyHz)
	}
}

// TestCompile_OnEvent_DeterministicSceneVersion is ADR 008 criterion #6:
// two compiles of the same on-event blueprint produce the same
// scene_version (topics are sorted before hashing).
func TestCompile_OnEvent_DeterministicSceneVersion(t *testing.T) {
	_, v1 := compileOnEvent(t, "goal")
	_, v2 := compileOnEvent(t, "goal")
	if v1 != v2 {
		t.Fatalf("scene_version not deterministic: %q vs %q", v1, v2)
	}
	if v1 == "" {
		t.Fatal("empty scene_version")
	}
}
