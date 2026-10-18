package bluehost

import "testing"

func TestAuthoritySwapKeepsInstanceHandlesAndState(t *testing.T) {
	h := NewHost()
	program := fixture(t)
	if err := h.Prepare(SlotPreview, "preview-actor", "scene-b", "digest-b", program, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Take("program-actor", "scene-a", "digest-a", program, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = h.Step(SlotPreview)
	_, _ = h.Step(SlotOnAir)
	p, v := h.slots[SlotOnAir], h.slots[SlotPreview]
	if err := h.SwapAuthority(); err != nil {
		t.Fatal(err)
	}
	if h.slots[SlotOnAir] != v || h.slots[SlotPreview] != p {
		t.Fatal("actors replaced")
	}
	if _, err := h.Step(SlotOnAir); err != nil {
		t.Fatal(err)
	}
	if err := h.SwapAuthority(); err != nil {
		t.Fatal(err)
	}
	if h.slots[SlotOnAir] != p || h.slots[SlotPreview] != v {
		t.Fatal("round trip lost actors")
	}
}
func TestAuthoritySwapRefusesSceneReplacementAndKeepsSlots(t *testing.T) {
	h := NewHost()
	_ = h.PreparePreviewStatic("scene-b", "v")
	p := h.slots[SlotPreview]
	h.transitions = map[Slot]bool{SlotOnAir: true}
	if h.SwapAuthority() == nil || h.slots[SlotPreview] != p || h.slots[SlotOnAir] != nil {
		t.Fatal("busy authority changed")
	}
}
