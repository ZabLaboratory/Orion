package bluehost

import (
	"errors"
	"testing"
)

func TestReplacementRetainsExactInstanceAndBlocksNewExecutions(t *testing.T) {
	h := NewHost()
	program := fixture(t)
	if err := h.Take("old", "scene-old", "old-digest", program, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.PreparePreview("preview", "preview", "preview-digest", program, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Step(SlotOnAir); err != nil {
		t.Fatal(err)
	}
	previous := h.slots[SlotOnAir]
	replacement, err := h.StageReplacement(SlotOnAir, "new", "scene-new", "new-digest", program, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.slots[SlotOnAir] != previous {
		t.Fatal("preparation replaced the active instance")
	}
	if _, err = h.Step(SlotOnAir); !errors.Is(err, ErrSceneChanging) {
		t.Fatalf("staged candidate stepped: %v", err)
	}
	if _, err = h.Call(SlotOnAir, "anything", nil); !errors.Is(err, ErrSceneChanging) {
		t.Fatalf("operator raced transition: %v", err)
	}
	if _, err = h.Step(SlotPreview); err != nil {
		t.Fatalf("other lane blocked: %v", err)
	}
	if err = replacement.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = replacement.Rollback(); err != nil {
		t.Fatal(err)
	}
	if h.slots[SlotOnAir] != previous {
		t.Fatal("rollback recreated the old instance")
	}
	if _, err = h.Step(SlotOnAir); err != nil {
		t.Fatalf("retained instance no longer usable: %v", err)
	}
	if err := h.Release(SlotOnAir, "test"); err != nil {
		t.Fatal(err)
	}
	if err := h.Release(SlotPreview, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestReplacementFinalizationAndFailedProgramPreserveLane(t *testing.T) {
	h := NewHost()
	if err := h.TakeStatic("old", "old-version"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StageReplacement(SlotOnAir, "bad", "bad", "bad", []byte(`{}`), nil, nil, nil); err == nil {
		t.Fatal("invalid program admitted")
	}
	if !h.Serving(SlotOnAir, "old", "old-version") {
		t.Fatal("failed load displaced old slot")
	}
	replacement, err := h.StageReplacement(SlotOnAir, "new", "new", "version", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = replacement.Finalize(); err == nil {
		t.Fatal("uncommitted stage finalized")
	}
	if err = replacement.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = replacement.Finalize(); err != nil {
		t.Fatal(err)
	}
	if !h.Serving(SlotOnAir, "new", "version") {
		t.Fatal("new scene missing")
	}
	if err = replacement.Rollback(); err != nil {
		t.Fatal("closed transaction was not idempotent")
	}
}
