package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// The 4th-link fix (ADR 013 issue 4 / quasar finale reactive scene): a
// pure-dataflow scene reads the show event the stream-level rule emits
// (`__events.stream_chat_event`) DIRECTLY via a `core.input` leaf node,
// instead of the `payload` data-out pin of an `on-event` exec entry. That
// pin is bound only in a fired exec task's transient env and is never a
// state leaf, so in a spine-less dataflow scene nothing consumed the
// `__events.*` write and the display leaf stayed null. Reading the leaf
// directly (the proven M1 shape) closes the link.
//
// This guards the runtime mechanism the fix relies on, end to end:
//
//   - A system `__events.stream_chat_event` write (the EmitToActive path,
//     IsSystem: true) reaches state and seeds the dirty cone (applyInput).
//   - The `core.input` leaf node is registered as the upstream of the
//     get-field's `record` edge, so the get-field is a CONSUMER of
//     `__events.stream_chat_event` (the reverse-adjacency index keys on the
//     input node's leaf path, scene.go upstreamPath / consumers).
//   - recompute wakes the get-field → output chain on that write, and the
//     output leaf `chat.display` carries the extracted `payload.text`.
//
// The leaf address is unprefixed (`__events.stream_chat_event`), matching a
// single-blueprint scene compiled under the legacy key "" — byte-identical
// to what EmitToActive writes. No compiler change is needed for that case.
func TestScene_EventsLeafInputDrivesReactiveDataflow(t *testing.T) {
	const eventLeaf = "__events.stream_chat_event"
	const displayLeaf = "chat.display"

	// The finale reactive scene graph (pure dataflow, no exec):
	//   core.input(__events.stream_chat_event)
	//     → get-field("payload.text")
	//     → core.output(chat.display)
	graph := &compiler.Graph{
		SceneID:      "scene-events-input",
		SceneVersion: "sha256:test",
		Nodes: []compiler.GraphNode{
			// Leaf input — reads the emitted event. Kind "input", its Path
			// IS the global event leaf (unprefixed, legacy key "").
			{ID: "chatIn", Kind: "input", Path: eventLeaf},
			// get-field("payload.text") consuming the input leaf.
			{
				ID:       "text",
				Kind:     "computed",
				Compute:  "core.data.get-field@1",
				Upstream: []string{"chatIn"},
				Inputs:   []compiler.GraphInput{{From: "chatIn", Port: "record"}},
				Config:   map[string]json.RawMessage{"path": json.RawMessage(`"payload.text"`)},
			},
			// Output sink writing the extracted text to the display leaf.
			{
				ID:       "out",
				Kind:     "output",
				Path:     displayLeaf,
				Compute:  "core.output@1",
				Upstream: []string{"text"},
				Inputs:   []compiler.GraphInput{{From: "text", Port: "value"}},
			},
		},
		Defaults: map[string]json.RawMessage{
			// Cold-start display leaf — null until the first chat lands.
			displayLeaf: json.RawMessage(`null`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:test"}
	scene := NewScene("scene-events-input", graph, bundle, NewComputeRegistry(), quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scene.Run(ctx)
	t.Cleanup(scene.Stop)

	sub, _ := scene.Subscribe(8)

	// The canonical chat event EmitToActive injects: a system write to
	// `__events.stream_chat_event` carrying `{type, payload:{text}}`.
	event := json.RawMessage(`{"type":"chat","payload":{"text":"DIAGTEST123"}}`)
	if !scene.Input(InputMsg{
		Path:     eventLeaf,
		Value:    event,
		Source:   "system:show.emit",
		IsSystem: true,
	}) {
		t.Fatal("inbox full")
	}

	// The reactive engine must wake the get-field → output chain and emit a
	// delta carrying `chat.display = "DIAGTEST123"`.
	deadline := time.After(time.Second)
	for {
		select {
		case msg := <-sub.Out:
			d, ok := msg.(*protocol.Delta)
			if !ok {
				continue
			}
			for _, p := range d.Patches {
				if p.Path != displayLeaf {
					continue
				}
				var got string
				if err := json.Unmarshal(p.Value, &got); err != nil {
					t.Fatalf("chat.display value not a string: %s (%v)", p.Value, err)
				}
				if got != "DIAGTEST123" {
					t.Fatalf("chat.display = %q, want %q — the dataflow read did not extract payload.text", got, "DIAGTEST123")
				}
				return // the 4th link is closed — pass
			}
		case <-deadline:
			t.Fatal("no chat.display delta after the __events write — the dataflow read on __events.* never woke (4th link broken)")
		}
	}
}
