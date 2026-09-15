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

func TestPreviewSlotReactivatesWarmEditableCloneAcrossRegularScene(t *testing.T) {
	wire := &editablePreviewWire{out: make(chan SubscriberMsg, 8)}
	slot := NewPreviewSlot(
		context.Background(),
		NewComputeRegistry(),
		wire,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer slot.Close()

	editableGraph := &compiler.Graph{
		SceneID:      "editable-a",
		SceneVersion: "sha256:editable-a",
		Defaults: map[string]json.RawMessage{
			"__editable.61.x": json.RawMessage(`10`),
		},
	}
	editableBundle := &compiler.RenderBundle{SceneVersion: editableGraph.SceneVersion}
	slot.ActivateEditable("editable-a", editableGraph, editableBundle, 4)
	warm := slot.Current()

	if err := slot.ApplyEditablePatches("editable-a", 4, 5, []EditablePatch{{
		Path: "__editable.61.x", Value: json.RawMessage(`35`),
	}}); err != nil {
		t.Fatalf("hot patch before switch: %v", err)
	}

	regularGraph := &compiler.Graph{SceneID: "blue-b", SceneVersion: "sha256:blue-b"}
	regularBundle := &compiler.RenderBundle{SceneVersion: regularGraph.SceneVersion}
	slot.Activate("blue-b", regularGraph, regularBundle)
	if slot.CurrentSceneID() != "blue-b" {
		t.Fatalf("current after regular activation = %q", slot.CurrentSceneID())
	}
	if _, ok := slot.Bundle("editable-a", editableGraph.SceneVersion); !ok {
		t.Fatal("warm editable bundle disappeared while Blue owned Preview")
	}
	if err := slot.ReactivateEditable("editable-a", 4); err != ErrPreviewEditSequence {
		t.Fatalf("stale durable head = %v, want %v", err, ErrPreviewEditSequence)
	}
	if slot.CurrentSceneID() != "blue-b" {
		t.Fatal("a rejected warm activation changed Preview")
	}
	if err := slot.ReactivateEditable("editable-a", 5); err != nil {
		t.Fatalf("ReactivateEditable: %v", err)
	}
	if slot.Current() != warm {
		t.Fatal("editable reactivation rebuilt the runtime clone")
	}
	if got := wire.mirrorIDs; len(got) != 2 || got[0] != "editable-a" || got[1] != "blue-b" {
		t.Fatalf("MirrorFor calls = %#v, want exactly editable-a then blue-b", got)
	}
	if got := wire.activeIDs; len(got) != 3 || got[0] != "editable-a" || got[1] != "blue-b" || got[2] != "editable-a" {
		t.Fatalf("active switches = %#v", got)
	}
	if got := wire.dropped; len(got) != 1 || got[0] != "blue-b" {
		t.Fatalf("drops before close = %#v, want disposable Blue clone only", got)
	}
}
