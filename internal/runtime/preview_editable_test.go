package runtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

type editablePreviewMirror struct{ out chan SubscriberMsg }

func (m editablePreviewMirror) Forward(msg SubscriberMsg) { m.out <- msg }

type editablePreviewWire struct {
	out       chan SubscriberMsg
	activeIDs []string
	mirrorIDs []string
	dropped   []string
}

type editableAirWire struct {
	out       chan SubscriberMsg
	mirrorIDs []string
	owners    []string
	active    []string
}

func (w *editableAirWire) MirrorForLSML(sceneID, _ string, owner string, _ []byte) SceneMirror {
	w.mirrorIDs = append(w.mirrorIDs, sceneID)
	w.owners = append(w.owners, owner)
	return editablePreviewMirror{out: w.out}
}

func (w *editableAirWire) SetActive(sceneID, _ string) { w.active = append(w.active, sceneID) }

func (w *editablePreviewWire) MirrorFor(sceneID string, _ string, _ *compiler.RenderBundle) SceneMirror {
	w.mirrorIDs = append(w.mirrorIDs, sceneID)
	return editablePreviewMirror{out: w.out}
}
func (w *editablePreviewWire) SetActive(id string) { w.activeIDs = append(w.activeIDs, id) }
func (w *editablePreviewWire) Drop(id string)      { w.dropped = append(w.dropped, id) }

func TestPreviewSlotEditablePatchIsAtomicAndSequenceChecked(t *testing.T) {
	wire := &editablePreviewWire{out: make(chan SubscriberMsg, 8)}
	slot := NewPreviewSlot(
		context.Background(),
		NewComputeRegistry(),
		wire,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer slot.Close()

	graph := &compiler.Graph{
		SceneID:      "editable-a",
		SceneVersion: "sha256:test",
		Defaults: map[string]json.RawMessage{
			"__editable.61.x": json.RawMessage(`10`),
			"__editable.61.y": json.RawMessage(`20`),
		},
	}
	bundle := &compiler.RenderBundle{SceneVersion: graph.SceneVersion}
	slot.ActivateEditable("editable-a", graph, bundle, 4)
	served, ok := slot.Bundle("editable-a", graph.SceneVersion)
	if !ok {
		t.Fatal("editable preview bundle was not exposed for Solar")
	}
	var servedBundle compiler.RenderBundle
	if err := json.Unmarshal(served, &servedBundle); err != nil {
		t.Fatalf("served bundle is invalid: %v", err)
	}
	if servedBundle.SceneVersion != graph.SceneVersion {
		t.Fatalf("served bundle version = %q, want %q", servedBundle.SceneVersion, graph.SceneVersion)
	}
	if _, ok := slot.Bundle("editable-a", "sha256:wrong"); ok {
		t.Fatal("preview bundle resolved for the wrong version")
	}

	// SetMirror seed.
	select {
	case <-wire.out:
	case <-time.After(time.Second):
		t.Fatal("preview mirror was not seeded")
	}

	err := slot.ApplyEditablePatches("editable-a", 4, 5, []EditablePatch{
		{Path: "__editable.61.x", Value: json.RawMessage(`35`)},
		{Path: "__editable.61.y", Value: json.RawMessage(`45`)},
	})
	if err != nil {
		t.Fatalf("ApplyEditablePatches: %v", err)
	}

	select {
	case got := <-wire.out:
		delta, ok := got.(*protocol.Delta)
		if !ok {
			t.Fatalf("message type = %T, want *protocol.Delta", got)
		}
		changes := map[string]string{}
		for _, patch := range delta.Patches {
			changes[patch.Path] = string(patch.Value)
		}
		if len(changes) != 2 || changes["__editable.61.x"] != "35" || changes["__editable.61.y"] != "45" {
			t.Fatalf("atomic changes = %#v", delta.Patches)
		}
	case <-time.After(time.Second):
		t.Fatal("no editable preview delta")
	}

	if err := slot.ApplyEditablePatches("editable-a", 4, 5, []EditablePatch{{Path: "__editable.61.x", Value: json.RawMessage(`36`)}}); err != ErrPreviewEditSequence {
		t.Fatalf("stale sequence error = %v, want %v", err, ErrPreviewEditSequence)
	}
	if err := slot.ApplyEditablePatches("editable-a", 5, 6, []EditablePatch{{Path: "program.x", Value: json.RawMessage(`36`)}}); err != ErrPreviewEditPath {
		t.Fatalf("undeclared path error = %v, want %v", err, ErrPreviewEditPath)
	}
	if len(wire.activeIDs) != 1 || wire.activeIDs[0] != "editable-a" {
		t.Fatalf("preview activation = %#v", wire.activeIDs)
	}
}

func TestPreviewSlotInputUpdatesEditableCloneWithoutAdvancingEditSequence(t *testing.T) {
	wire := &editablePreviewWire{out: make(chan SubscriberMsg, 8)}
	slot := NewPreviewSlot(
		context.Background(),
		NewComputeRegistry(),
		wire,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer slot.Close()

	graph := &compiler.Graph{
		SceneID:      "editable-chat",
		SceneVersion: "sha256:editable-chat",
		Defaults: map[string]json.RawMessage{
			"overlay_message": json.RawMessage(`"Waiting for messages…"`),
		},
	}
	slot.ActivateEditable("editable-chat", graph, &compiler.RenderBundle{SceneVersion: graph.SceneVersion}, 0)
	select {
	case <-wire.out:
	case <-time.After(time.Second):
		t.Fatal("preview mirror was not seeded")
	}
	if err := slot.ApplyPreviewInput("overlay_message", json.RawMessage(`"chat: hello"`), "service:prism-local-quasar"); err != nil {
		t.Fatalf("ApplyPreviewInput: %v", err)
	}
	select {
	case got := <-wire.out:
		delta, ok := got.(*protocol.Delta)
		if !ok {
			t.Fatalf("message type = %T, want *protocol.Delta", got)
		}
		if len(delta.Patches) != 1 || delta.Patches[0].Path != "overlay_message" || string(delta.Patches[0].Value) != `"chat: hello"` {
			t.Fatalf("preview input delta = %#v", delta.Patches)
		}
	case <-time.After(time.Second):
		t.Fatal("no preview input delta")
	}
	// The service event is not a ZabCanvas edit. The next authoring patch must
	// still extend the original sequence zero → one.
	if err := slot.ApplyEditablePatches("editable-chat", 0, 1, []EditablePatch{{
		Path: "overlay_message", Value: json.RawMessage(`"manual"`),
	}}); err != nil {
		t.Fatalf("input advanced edit sequence: %v", err)
	}
}

func TestPreviewSlotReleasesInactiveEditableClone(t *testing.T) {
	wire := &editablePreviewWire{out: make(chan SubscriberMsg, 128)}
	slot := NewPreviewSlot(context.Background(), NewComputeRegistry(), wire, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slot.Close()
	graph := &compiler.Graph{SceneID: "a", SceneVersion: "sha256:a"}
	slot.ActivateEditable("a", graph, &compiler.RenderBundle{SceneVersion: graph.SceneVersion}, 0)
	first := slot.Current()
	slot.Activate("b", &compiler.Graph{SceneID: "b", SceneVersion: "sha256:b"}, &compiler.RenderBundle{SceneVersion: "sha256:b"})
	if _, ok := slot.Bundle("a", graph.SceneVersion); ok {
		t.Fatal("inactive bundle retained")
	}
	if err := slot.ReactivateEditable("a", 0); err != ErrPreviewCacheMiss {
		t.Fatalf("reactivation = %v", err)
	}
	slot.ActivateEditable("a", graph, &compiler.RenderBundle{SceneVersion: graph.SceneVersion}, 0)
	if slot.Current() == first {
		t.Fatal("reused inactive clone")
	}
	slot.ReleasePreview()
	if slot.Current() != nil {
		t.Fatal("external takeover retained clone")
	}
	if err := slot.ReactivateEditable("a", 0); err != ErrPreviewCacheMiss {
		t.Fatalf("external reactivation = %v", err)
	}
	if len(wire.dropped) != 3 {
		t.Fatalf("drops = %#v", wire.dropped)
	}
}

func TestPreviewSlotPromotesEditableCloneToGenerationWithoutTouchingPreview(t *testing.T) {
	previewWire := &editablePreviewWire{out: make(chan SubscriberMsg, 8)}
	airWire := &editableAirWire{out: make(chan SubscriberMsg, 8)}
	slot := NewPreviewSlot(
		context.Background(),
		NewComputeRegistry(),
		previewWire,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	slot.SetEditableAirWire(airWire)
	defer slot.Close()

	graph := &compiler.Graph{
		SceneID:      "editable-air",
		SceneVersion: "sha256:editable-air",
		Defaults: map[string]json.RawMessage{
			"__editable.61.x": json.RawMessage(`10`),
		},
	}
	slot.ActivateEditableWithBundle(
		"editable-air",
		graph,
		&compiler.RenderBundle{SceneVersion: graph.SceneVersion},
		3,
		[]byte(`{"lsml":"1.1","scene_id":"editable-air","scene_version":"sha256:editable-air","layout":{"kind":"stack"}}`),
	)
	select {
	case <-previewWire.out:
	case <-time.After(time.Second):
		t.Fatal("preview mirror was not seeded")
	}
	version, err := slot.PromoteEditable("editable-air", "on-air")
	if err != nil {
		t.Fatalf("PromoteEditable: %v", err)
	}
	if version != graph.SceneVersion {
		t.Fatalf("generation version = %q, want %q", version, graph.SceneVersion)
	}
	if len(airWire.mirrorIDs) != 1 || airWire.mirrorIDs[0] != "editable-air" {
		t.Fatalf("generation mirror calls = %#v", airWire.mirrorIDs)
	}
	if len(airWire.owners) != 1 || airWire.owners[0] != "on-air" {
		t.Fatalf("generation owner = %#v", airWire.owners)
	}
	if len(airWire.active) != 1 || airWire.active[0] != "editable-air" {
		t.Fatalf("generation active = %#v", airWire.active)
	}
	if err := slot.ApplyEditablePatches("editable-air", 3, 4, []EditablePatch{{
		Path: "__editable.61.x", Value: json.RawMessage(`55`),
	}}); err != nil {
		t.Fatalf("ApplyEditablePatches after promotion: %v", err)
	}
	select {
	case got := <-airWire.out:
		if _, ok := got.(*protocol.Snapshot); !ok {
			t.Fatalf("generation seed type = %T, want *protocol.Snapshot", got)
		}
	case <-time.After(time.Second):
		t.Fatal("generation mirror did not receive promotion seed")
	}
	select {
	case got := <-airWire.out:
		if _, ok := got.(*protocol.Delta); !ok {
			t.Fatalf("generation message type = %T, want *protocol.Delta", got)
		}
	case <-time.After(time.Second):
		t.Fatal("generation mirror did not receive editable delta")
	}
}

func TestPreviewSlotServiceInputFansOutToPromotedGeneration(t *testing.T) {
	previewWire := &editablePreviewWire{out: make(chan SubscriberMsg, 8)}
	airWire := &editableAirWire{out: make(chan SubscriberMsg, 8)}
	slot := NewPreviewSlot(
		context.Background(),
		NewComputeRegistry(),
		previewWire,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	slot.SetEditableAirWire(airWire)
	defer slot.Close()

	graph := &compiler.Graph{
		SceneID:      "editable-air-input",
		SceneVersion: "sha256:editable-air-input",
		Defaults: map[string]json.RawMessage{
			"overlay_message": json.RawMessage(`"Waiting"`),
		},
	}
	slot.ActivateEditableWithBundle(
		"editable-air-input",
		graph,
		&compiler.RenderBundle{SceneVersion: graph.SceneVersion},
		0,
		[]byte(`{"lsml":"1.1","scene_id":"editable-air-input","scene_version":"sha256:editable-air-input","layout":{"kind":"stack"}}`),
	)
	select {
	case <-previewWire.out:
	case <-time.After(time.Second):
		t.Fatal("preview mirror was not seeded")
	}
	if _, err := slot.PromoteEditable("editable-air-input", "on-air"); err != nil {
		t.Fatalf("PromoteEditable: %v", err)
	}
	// Drain the generation snapshot emitted during promotion. The next frame
	// must be the service input delta on the same immutable generation wire.
	select {
	case got := <-airWire.out:
		if _, ok := got.(*protocol.Snapshot); !ok {
			t.Fatalf("generation seed type = %T, want *protocol.Snapshot", got)
		}
	case <-time.After(time.Second):
		t.Fatal("generation mirror did not receive promotion seed")
	}
	if err := slot.ApplyPreviewInput("overlay_message", json.RawMessage(`"chat: live"`), "service:prism-local-quasar"); err != nil {
		t.Fatalf("ApplyPreviewInput after promotion: %v", err)
	}
	select {
	case got := <-airWire.out:
		delta, ok := got.(*protocol.Delta)
		if !ok {
			t.Fatalf("generation input type = %T, want *protocol.Delta", got)
		}
		if len(delta.Patches) != 1 || delta.Patches[0].Path != "overlay_message" || string(delta.Patches[0].Value) != `"chat: live"` {
			t.Fatalf("generation input delta = %#v", delta.Patches)
		}
	case <-time.After(time.Second):
		t.Fatal("generation mirror did not receive service input delta")
	}
}

func TestPreviewSlotRetainsOnlyExplicitActiveProgramGeneration(t *testing.T) {
	wire := &editablePreviewWire{out: make(chan SubscriberMsg, 128)}
	air := &editableAirWire{out: make(chan SubscriberMsg, 128)}
	slot := NewPreviewSlot(context.Background(), NewComputeRegistry(), wire, slog.New(slog.NewTextHandler(io.Discard, nil)))
	slot.SetEditableAirWire(air)
	defer slot.Close()
	graph := &compiler.Graph{SceneID: "program", SceneVersion: "sha256:program"}
	slot.ActivateEditableWithBundle("program", graph, &compiler.RenderBundle{SceneVersion: graph.SceneVersion}, 0, []byte(`{"lsml":"1.1","scene_id":"program","scene_version":"sha256:program","layout":{"kind":"stack"}}`))
	program := slot.Current()
	if _, err := slot.PromoteEditable("program", "on-air"); err != nil {
		t.Fatal(err)
	}
	slot.ActivateEditable("next", &compiler.Graph{SceneID: "next", SceneVersion: "sha256:next"}, &compiler.RenderBundle{SceneVersion: "sha256:next"}, 0)
	select {
	case <-program.done:
		t.Fatal("Preview change stopped the active Program")
	default:
	}
	if _, ok := slot.Bundle("program", "sha256:program"); !ok {
		t.Fatal("active Program bundle was removed")
	}
	if err := slot.ReactivateEditable("program", 0); err != ErrPreviewCacheMiss {
		t.Fatalf("inactive Preview reactivation = %v", err)
	}
	current := slot.Current()
	slot.ReleaseEditableAir()
	select {
	case <-program.done:
	case <-time.After(time.Second):
		t.Fatal("retired Program did not stop")
	}
	if _, ok := slot.Bundle("program", "sha256:program"); ok {
		t.Fatal("retired Program bundle retained")
	}
	if slot.Current() != current {
		t.Fatal("Program release changed Preview")
	}
	select {
	case <-current.done:
		t.Fatal("Program release stopped Preview")
	default:
	}
}
