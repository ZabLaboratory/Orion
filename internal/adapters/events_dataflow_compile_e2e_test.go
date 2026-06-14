package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// finaleFetcher is the in-memory compiler.Fetcher for the end-to-end
// compile-from-push proof: it serves the exact envelope shape the quasar
// finale reactive scene pushes (single blueprint, legacy key "", a
// `core.input` on `__events.stream_chat_event` → get-field → output).
type finaleFetcher struct{}

func (finaleFetcher) FetchCanvasLayout(_ context.Context, _ string) (*compiler.CanvasLayout, error) {
	return &compiler.CanvasLayout{
		Version: "v1",
		Root:    compiler.LayoutNode{Kind: "stack", ID: "root"},
	}, nil
}

func (finaleFetcher) FetchBlueprint(_ context.Context, _ string) (*compiler.BlueprintGraph, error) {
	return &compiler.BlueprintGraph{
		ID: "bp-quasar-finale-reactive-scene",
		Nodes: []compiler.BlueprintNode{
			{ID: "chatIn", Compute: "core.input@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"__events.stream_chat_event"`)}},
			{ID: "text", Compute: "core.data.get-field@1",
				Config: map[string]json.RawMessage{"path": json.RawMessage(`"payload.text"`)}},
			{ID: "out", Compute: "core.output@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"chat.display"`)}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "chatIn", ToNode: "text", FromPort: "value", ToPort: "record"},
			{FromNode: "text", ToNode: "out", FromPort: "value", ToPort: "value"},
		},
	}, nil
}

func (finaleFetcher) FetchBlueprintGraph(_ context.Context, _ string, _ int) (*compiler.ResolvedBlueprintGraph, error) {
	return nil, errors.New("no blueprint references")
}

func (finaleFetcher) FetchComponent(_ context.Context, _ compiler.ComponentRef) (*compiler.UserComponent, error) {
	return nil, errors.New("no components")
}

func (finaleFetcher) FetchComputeManifest(_ context.Context) (compiler.ComputeManifest, error) {
	return compiler.ComputeManifest{
		"core.input@1":          {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":         {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.get-field@1": {IsPure: true, IsBounded: true, Version: "1"},
	}, nil
}

// TestFinaleReactive_CompileFromPushDrivesChatDisplay is the quasar-finale
// 5th-link end-to-end regression (ADR 013), exercising the compile-FROM-PUSH
// path the in-memory #175 runtime test never touched.
//
// It COMPILES the real envelope (single blueprint, legacy key "", a
// `core.input` leaf on `__events.stream_chat_event` → get-field("payload.text")
// → core.output("chat.display")), loads the COMPILED graph into a live Show,
// activates it, then drives a service write to `__events.stream_chat_event`
// through the REAL inbox — the production wire path, gated by sceneAcceptsPath.
//
// Before the fix the compiler synthesized NO acceptance binding for the
// `__events.*` leaf a pure-dataflow input reads (eventTopicBindings was fed
// only by on-event exec entrypoints), so sceneAcceptsPath REJECTED this write
// → the leaf was never written → the dataflow cone never woke → `chat.display`
// stayed null at the antenna (the empirically-observed 0-delta symptom).
//
// The pass criterion is the antenna contract: the write yields a delta
// `chat.display = "<the chat text>"`.
func TestFinaleReactive_CompileFromPushDrivesChatDisplay(t *testing.T) {
	const eventLeaf = "__events.stream_chat_event"
	const displayLeaf = "chat.display"
	const sceneID = "scene-quasar-finale"

	graph, bundle, _, err := compiler.Compile(context.Background(), sceneID,
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-quasar-finale-reactive-scene"},
		finaleFetcher{})
	if err != nil {
		t.Fatalf("compile-from-push failed: %v", err)
	}

	show := runtime.NewShow(runtime.NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.Load(sceneID, graph, bundle)
	if err := show.SetActive(sceneID, nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	scene, _ := show.Get(sceneID)
	sub, _ := scene.Subscribe(16)

	inbox := NewInbox(show, quietLogger(), nil)

	// A service/operator write to the event leaf — the production wire path,
	// gated by sceneAcceptsPath. An operator may write any non-__system path.
	operator := identityForTest("operator", "stream-rule", nil)
	if err := inbox.Write(context.Background(), Write{
		Identity: operator,
		Path:     eventLeaf,
		Value:    json.RawMessage(`{"type":"chat","payload":{"text":"DIAGTEST123"}}`),
		Source:   "operator:test",
	}); err != nil {
		t.Fatalf("write to %s refused: %v", eventLeaf, err)
	}

	deadline := time.After(2 * time.Second)
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
					t.Fatalf("%s value not a string: %s (%v)", displayLeaf, p.Value, err)
				}
				// The scene's cold-start recompute emits a `chat.display`
				// patch (empty — the get-field over a null record) at load,
				// which can race ahead of the event write onto sub.Out. It is
				// NOT the assertion target: keep reading until the WRITE drives
				// the extracted text through. Without the acceptance binding
				// fix that write is dropped, so the deadline fires (the bug).
				if got == "" {
					continue
				}
				if got != "DIAGTEST123" {
					t.Fatalf("%s = %q, want %q", displayLeaf, got, "DIAGTEST123")
				}
				return // the 5th link is closed — compile-from-push drives the antenna
			}
		case <-deadline:
			t.Fatalf("no %s delta after the %s write — the compiled scene rejected the write (no acceptance binding) so the reactive cone never woke (the finale 5th-link bug)", displayLeaf, eventLeaf)
		}
	}
}
