package bluehost

import (
	"errors"
	"testing"
)

// Static occupations (ORION-NOBLUE-AND-VERSION-ALIGN, #398): a slot
// occupied with no runtime instance keeps Prepare's admission contract
// and answers every instance door with ErrNotLoaded — see entry.instance.

func TestPrepareStatic_AdmissionAndIdentity(t *testing.T) {
	h := NewHost()
	if err := h.PrepareStatic(SlotPreview, "scene-1", "sha256:v1"); err != nil {
		t.Fatalf("PrepareStatic: %v", err)
	}
	if !h.Serving(SlotPreview, "scene-1", "sha256:v1") {
		t.Fatal("expected Serving (scene-1, sha256:v1)")
	}
	// Same admission as Prepare: an occupied slot is refused.
	if err := h.PrepareStatic(SlotPreview, "scene-2", "sha256:v2"); !errors.Is(err, ErrAlreadyLoaded) {
		t.Fatalf("expected ErrAlreadyLoaded, got %v", err)
	}
	// Release on a static occupation: nothing to stop, no error.
	if err := h.Release(SlotPreview, "test"); err != nil {
		t.Fatalf("Release of a static occupation: %v", err)
	}
	if h.Digest(SlotPreview) != "" {
		t.Fatal("expected the slot to be empty after Release")
	}
}

func TestPreparePreviewStaticReplacesDifferentScene(t *testing.T) {
	h := NewHost()
	if err := h.PreparePreviewStatic("scene-1", "sha256:v1"); err != nil {
		t.Fatalf("first PreparePreviewStatic: %v", err)
	}
	if err := h.PreparePreviewStatic("scene-2", "sha256:v2"); err != nil {
		t.Fatalf("replacement PreparePreviewStatic: %v", err)
	}
	if !h.Serving(SlotPreview, "scene-2", "sha256:v2") {
		t.Fatal("preview slot does not serve the replacement static scene")
	}
	if h.Serving(SlotPreview, "scene-1", "sha256:v1") {
		t.Fatal("preview slot still serves the superseded static scene")
	}
}

func TestTakeStatic_SupersedesStaticWithoutError(t *testing.T) {
	h := NewHost()
	if err := h.TakeStatic("scene-1", "sha256:v1"); err != nil {
		t.Fatalf("first TakeStatic: %v", err)
	}
	if err := h.TakeStatic("scene-2", "sha256:v2"); err != nil {
		t.Fatalf("TakeStatic superseding a static occupation must not error: %v", err)
	}
	if !h.Serving(SlotOnAir, "scene-2", "sha256:v2") {
		t.Fatal("expected on-air re-keyed to (scene-2, sha256:v2)")
	}
}

func TestStaticOccupation_InstanceDoorsAnswerNotLoaded(t *testing.T) {
	h := NewHost()
	if err := h.TakeStatic("scene-1", "sha256:v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Dispatch(SlotOnAir, []byte(`{}`)); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Dispatch: expected ErrNotLoaded, got %v", err)
	}
	if _, err := h.Tick(SlotOnAir, 0.1); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Tick: expected ErrNotLoaded, got %v", err)
	}
	if _, err := h.Resolve(SlotOnAir, "x", "v"); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Resolve: expected ErrNotLoaded, got %v", err)
	}
	if _, err := h.Complete(SlotOnAir, []byte(`{}`)); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Complete: expected ErrNotLoaded, got %v", err)
	}
	if _, err := h.WritePlatformEvent(SlotOnAir, "leaf", nil); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("WritePlatformEvent: expected ErrNotLoaded, got %v", err)
	}
	triggers, awaits := h.DeclaredContracts(SlotOnAir)
	if triggers != nil || awaits != nil {
		t.Fatal("static occupation declares no operator surface")
	}
	// Identity and bundle service still work — that IS the occupation.
	h.SetBundle(SlotOnAir, []byte(`{"scene_version":"sha256:v1"}`))
	if h.Bundle(SlotOnAir) == nil {
		t.Fatal("expected the bundle to be servable")
	}
}
