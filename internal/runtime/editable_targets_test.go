package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

func TestEditableTargetsFollowActorAndFenceStructuralSnapshots(t *testing.T) {
	wire := &editablePreviewWire{out: make(chan SubscriberMsg, 64)}
	program := &editablePreviewWire{out: make(chan SubscriberMsg, 64)}
	p := NewPreviewSlot(context.Background(), NewComputeRegistry(), wire, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.SetEditableProgramWire(program)
	defer p.Close()
	g := &compiler.Graph{SceneID: "a", SceneVersion: "v1", Defaults: map[string]json.RawMessage{"__editable.x": json.RawMessage(`10`)}}
	b := &compiler.RenderBundle{SceneVersion: "v1"}
	p.ActivateEditable("a", g, b, 4)
	head, _ := p.EditableTargetHead("preview")
	actor := p.Current()
	finish, err := p.StageRoleSwap("a")
	if err != nil {
		t.Fatal(err)
	}
	finish(true)
	air, ok := p.EditableTargetHead("program")
	if !ok || air != head {
		t.Fatal("Take changed author identity", air, head)
	}
	patches := []EditablePatch{{Path: "__editable.x", Value: json.RawMessage(`20`)}}
	if err = p.ApplyEditableTargetPatches("preview", "a", head.WriterID, 4, 5, patches); !errors.Is(err, ErrPreviewSceneMismatch) {
		t.Fatal("old role accepted", err)
	}
	if err = p.ApplyEditableTargetPatches("program", "a", "retired", 4, 5, patches); !errors.Is(err, ErrPreviewEditSequence) {
		t.Fatal("retired writer accepted", err)
	}
	fence, _ := p.EditableTargetSnapshot("program")
	if err = p.ApplyEditableTargetPatches("program", "a", head.WriterID, 4, 5, patches); err != nil {
		t.Fatal(err)
	}
	if err = p.FlushEditableTarget(context.Background(), "program"); err != nil {
		t.Fatal(err)
	}
	_, _, state := actor.SnapshotState()
	if string(state["__editable.x"]) != "20" {
		t.Fatal(state)
	}
	base := uint64(5)
	called := false
	before := func() error { called = true; return nil }
	if err = p.ReplaceEditableTarget("program", "a", head.WriterID, &base, fence, g, b, 6, nil, before); !errors.Is(err, ErrPreviewEditSequence) || called {
		t.Fatal("stale compilation mutated host", err, called)
	}
	head, _ = p.EditableTargetHead("program")
	fence, _ = p.EditableTargetSnapshot("program")
	if err = p.ReplaceEditableTarget("program", "other", head.WriterID, &base, fence, g, b, 6, nil, before); !errors.Is(err, ErrPreviewSceneMismatch) || called {
		t.Fatal("wrong scene mutated host", err, called)
	}
	if err = p.ReplaceEditableTarget("program", "a", head.WriterID, &base, fence, g, b, 6, nil, before); err != nil {
		t.Fatal(err)
	}
	next, _ := p.EditableTargetHead("program")
	if !called || next.WriterID == head.WriterID || next.EditSeq != 6 || program.activeIDs[0] != "a" {
		t.Fatal("wrong structural target", next, program.activeIDs)
	}
	if err = p.ApplyEditableTargetPatches("program", "a", head.WriterID, 6, 7, patches); !errors.Is(err, ErrPreviewEditSequence) {
		t.Fatal("old actor writer accepted after replacement", err)
	}
}

func TestEditableSnapshotFencesRoleSwapWithoutEditableActors(t *testing.T) {
	p := NewPreviewSlot(context.Background(), NewComputeRegistry(), &editablePreviewWire{out: make(chan SubscriberMsg, 8)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer p.Close()
	fence, _ := p.EditableTargetSnapshot("preview")
	finish, err := p.StageRoleSwap("blue")
	if err != nil {
		t.Fatal(err)
	}
	finish(true)
	called := false
	err = p.ReplaceEditableTarget("preview", "a", "", nil, fence, &compiler.Graph{SceneID: "a", SceneVersion: "v1"}, &compiler.RenderBundle{SceneVersion: "v1"}, 0, nil, func() error { called = true; return nil })
	if !errors.Is(err, ErrPreviewEditSequence) || called {
		t.Fatal("stale snapshot followed new physical role", err, called)
	}
}

func TestLegacyEditableGenerationIsNotNativeProgramAuthor(t *testing.T) {
	p := NewPreviewSlot(context.Background(), NewComputeRegistry(), &editablePreviewWire{out: make(chan SubscriberMsg, 16)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.SetEditableProgramWire(&editablePreviewWire{out: make(chan SubscriberMsg, 16)})
	p.SetEditableAirWire(&editableAirWire{out: make(chan SubscriberMsg, 16)})
	defer p.Close()
	g := &compiler.Graph{SceneID: "a", SceneVersion: "v1"}
	p.ActivateEditableWithBundle("a", g, &compiler.RenderBundle{SceneVersion: "v1"}, 0, []byte(`{"scene_id":"a"}`))
	if _, err := p.PromoteEditable("a", "legacy-generation"); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.EditableTargetHead("program"); ok {
		t.Fatal("generation exposed as physical Program")
	}
	p.ReleasePreview()
	if _, _, err := p.EditableTargetState(context.Background(), "program", "a"); !errors.Is(err, ErrPreviewSceneMismatch) {
		t.Fatal("generation read as active Program", err)
	}
}
