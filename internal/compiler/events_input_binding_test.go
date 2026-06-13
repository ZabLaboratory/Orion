package compiler

import (
	"context"
	"encoding/json"
	"testing"
)

// The quasar-finale 5th-link regression (ADR 013): a PURE-DATAFLOW reactive
// scene reads an `__events.*` topic through a `core.input@1` leaf node — the
// M1 shape, single blueprint, legacy key "", no on-event exec entry:
//
//	core.input(name="__events.stream_chat_event")
//	  --record--> get-field(path="payload.text")
//	  --value-->  core.output(chat.display)
//
// The bug: eventTopicBindings was fed ONLY by on-event exec entrypoints
// (eventTopics), so a spine-less dataflow scene reading the topic via a
// data input got NO `event-topic` acceptance binding. The inbox's
// sceneAcceptsPath then REJECTED a wire/service write to
// `__events.stream_chat_event` (no binding, no default, no operator_input)
// → the leaf was never written → the dataflow cone never woke →
// `chat.display` stayed null at the antenna.
//
// This exercises the compile-FROM-PUSH path the in-memory #175 runtime test
// never touched: it starts from the real envelope shape (single-blueprint,
// legacy key "", `core.input` on `__events.*` → get-field → output) and
// asserts the compiler synthesizes the acceptance binding the inbox gates on.
//
// Contrast with M1, which read `__inputs.platform.*` and worked: a quasar.*
// dataflow node got its acceptance binding from platformStreamBindings' node
// scan. There was no equivalent node scan for `__events.*` input leaves —
// the gap this fix closes (eventInputLeaves, the mirror of that scan).
func TestCompile_EventsDataflowInputSynthesizesAcceptanceBinding(t *testing.T) {
	const eventLeaf = "__events.stream_chat_event"

	bp := &BlueprintGraph{
		ID: "bp-quasar-finale-reactive",
		Nodes: []BlueprintNode{
			{ID: "chatIn", Compute: "core.input@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"` + eventLeaf + `"`)}},
			{ID: "text", Compute: "core.data.get-field@1",
				Config: map[string]json.RawMessage{"path": json.RawMessage(`"payload.text"`)}},
			outputNode("out", "chat.display"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "chatIn", ToNode: "text", FromPort: "value", ToPort: "record"},
			{FromNode: "text", ToNode: "out", FromPort: "value", ToPort: "value"},
		},
	}
	m := pureManifest()
	m["core.data.get-field@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-quasar-finale-reactive": bp},
		components: map[ComponentRef]*UserComponent{},
		manifest:   m,
	}

	g, _, _, err := Compile(context.Background(), "scene-finale",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-quasar-finale-reactive"}, f)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	// Edges materialize as Upstream/Inputs (this was never the bug, but
	// guard it so a future regression here is caught on the same path).
	byID := map[string]GraphNode{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	if up := byID["text"].Upstream; len(up) != 1 || up[0] != "chatIn" {
		t.Fatalf("get-field upstream = %v, want [chatIn] — record edge dropped", up)
	}
	if up := byID["out"].Upstream; len(up) != 1 || up[0] != "text" {
		t.Fatalf("output upstream = %v, want [text] — value edge dropped", up)
	}
	if p := byID["chatIn"].Path; p != eventLeaf {
		t.Fatalf("input leaf path = %q, want %q", p, eventLeaf)
	}

	// The fix: an `event-topic` acceptance binding for the `__events.*` leaf
	// the data input reads. Without it sceneAcceptsPath rejects the write.
	found := false
	for _, b := range g.Bindings {
		if b.Kind == "event-topic" {
			for _, tp := range b.TargetPaths {
				if tp == eventLeaf {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("no event-topic acceptance binding for %q — a wire/service write to it would be rejected by sceneAcceptsPath and the reactive cone would never wake (the finale 5th-link bug). bindings=%+v", eventLeaf, g.Bindings)
	}
}
