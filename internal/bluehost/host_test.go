package bluehost

import (
	"os"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../bluespike/testdata/01-minimal.program.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func TestHost_PreparePreviewAndOnAirAreIsolated(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if err := h.Prepare(SlotPreview, "preview-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare preview: %v", err)
	}
	if err := h.Take("onair-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("Take: %v", err)
	}

	if h.Digest(SlotPreview) != "sha256:aaa" {
		t.Fatalf("preview digest not tracked")
	}
	if h.Digest(SlotOnAir) != "sha256:aaa" {
		t.Fatalf("on-air digest not tracked")
	}

	if _, err := h.Step(SlotPreview); err != nil {
		t.Fatalf("Step preview: %v", err)
	}
	if _, err := h.Step(SlotOnAir); err != nil {
		t.Fatalf("Step on-air: %v", err)
	}
}

func TestHost_PrepareRefusesDoubleLoad(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if err := h.Prepare(SlotPreview, "preview-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := h.Prepare(SlotPreview, "preview-2", "sha256:bbb", program, nil, nil, nil); err == nil {
		t.Fatal("expected ErrAlreadyLoaded on second Prepare of the same slot")
	}
}

func TestHost_TakeSupersedesPreviousOnAirWithoutStateTransfer(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if err := h.Take("onair-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("first Take: %v", err)
	}
	if err := h.Take("onair-2", "sha256:bbb", program, nil, nil, nil); err != nil {
		t.Fatalf("second Take: %v", err)
	}
	if h.Digest(SlotOnAir) != "sha256:bbb" {
		t.Fatalf("expected on-air digest to be the superseding one, got %q", h.Digest(SlotOnAir))
	}
	// Only one on-air instance ever exists — Step on the (now-stopped)
	// first instance is unreachable via the Host API, which only ever
	// exposes the current slot occupant.
	if _, err := h.Step(SlotOnAir); err != nil {
		t.Fatalf("Step on superseding instance: %v", err)
	}
}

func TestHost_TakeFailureLeavesPreviousOnAirUntouched(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if err := h.Take("onair-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("first Take: %v", err)
	}

	if err := h.Take("onair-2", "sha256:bbb", []byte(`not-json`), nil, nil, nil); err == nil {
		t.Fatal("expected failure loading malformed program")
	}

	if h.Digest(SlotOnAir) != "sha256:aaa" {
		t.Fatalf("failed take must not disturb the committed on-air instance, got %q", h.Digest(SlotOnAir))
	}
	if _, err := h.Step(SlotOnAir); err != nil {
		t.Fatalf("Step on untouched on-air instance: %v", err)
	}
}

func TestHost_ReleaseIsIdempotentOnEmptySlot(t *testing.T) {
	h := NewHost()
	if err := h.Release(SlotPreview, "no-op"); err != nil {
		t.Fatalf("Release on empty slot must be a no-op, got %v", err)
	}
}

func TestHost_StepOnEmptySlotFails(t *testing.T) {
	h := NewHost()
	if _, err := h.Step(SlotPreview); err == nil {
		t.Fatal("expected ErrNotLoaded")
	}
}

func TestHost_ReleaseThenReprepare(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if err := h.Prepare(SlotPreview, "preview-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := h.Release(SlotPreview, "operator-cancelled"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if h.Digest(SlotPreview) != "" {
		t.Fatalf("expected empty slot after Release")
	}
	if err := h.Prepare(SlotPreview, "preview-2", "sha256:bbb", program, nil, nil, nil); err != nil {
		t.Fatalf("re-Prepare after Release: %v", err)
	}
}

func TestHost_SetBundleAndBundle(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if h.Bundle(SlotPreview) != nil {
		t.Fatal("expected nil bundle on empty slot")
	}
	h.SetBundle(SlotPreview, []byte("no-op on empty slot"))
	if h.Bundle(SlotPreview) != nil {
		t.Fatal("expected SetBundle on an empty slot to be a no-op")
	}

	if err := h.Prepare(SlotPreview, "preview-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.SetBundle(SlotPreview, []byte("lsml-bytes"))
	if got := h.Bundle(SlotPreview); string(got) != "lsml-bytes" {
		t.Fatalf("expected %q, got %q", "lsml-bytes", got)
	}

	if err := h.Release(SlotPreview, "done"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if h.Bundle(SlotPreview) != nil {
		t.Fatal("expected nil bundle after Release")
	}
}
