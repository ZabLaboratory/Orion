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

	if err := h.Prepare(SlotPreview, "preview-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare preview: %v", err)
	}
	if err := h.Take("onair-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
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

	if err := h.Prepare(SlotPreview, "preview-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := h.Prepare(SlotPreview, "preview-2", "scene-2", "sha256:bbb", program, nil, nil, nil); err == nil {
		t.Fatal("expected ErrAlreadyLoaded on second Prepare of the same slot")
	}
}

func TestHost_PreparePreviewReplacesDifferentScene(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if err := h.PreparePreview("preview-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("first PreparePreview: %v", err)
	}
	if err := h.PreparePreview("preview-2", "scene-2", "sha256:bbb", program, nil, nil, nil); err != nil {
		t.Fatalf("replacement PreparePreview: %v", err)
	}
	if !h.Serving(SlotPreview, "scene-2", "sha256:bbb") {
		t.Fatal("preview slot does not serve the replacement scene")
	}
	if h.Serving(SlotPreview, "scene-1", "sha256:aaa") {
		t.Fatal("preview slot still serves the superseded scene")
	}
}

func TestHost_ReusesParsedProgramHandleAcrossSceneReplacements(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if err := h.PreparePreview("preview-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("first PreparePreview: %v", err)
	}
	if err := h.PreparePreview("preview-2", "scene-2", "sha256:bbb", program, nil, nil, nil); err != nil {
		t.Fatalf("second PreparePreview: %v", err)
	}
	if got := len(h.programHandles); got != 1 {
		t.Fatalf("expected one cached parsed program, got %d", got)
	}
	if got := len(h.programHandleOrder); got != 1 {
		t.Fatalf("expected one cache order entry, got %d", got)
	}
	if got := len(h.programMetadata); got != 1 {
		t.Fatalf("expected one cached metadata entry, got %d", got)
	}
	if got := len(h.metadataOrder); got != 1 {
		t.Fatalf("expected one metadata cache order entry, got %d", got)
	}
}

func TestHost_TakeSupersedesPreviousOnAirWithoutStateTransfer(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if err := h.Take("onair-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("first Take: %v", err)
	}
	if err := h.Take("onair-2", "scene-1", "sha256:bbb", program, nil, nil, nil); err != nil {
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

	if err := h.Take("onair-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("first Take: %v", err)
	}

	if err := h.Take("onair-2", "scene-1", "sha256:bbb", []byte(`not-json`), nil, nil, nil); err == nil {
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

	if err := h.Prepare(SlotPreview, "preview-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := h.Release(SlotPreview, "operator-cancelled"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if h.Digest(SlotPreview) != "" {
		t.Fatalf("expected empty slot after Release")
	}
	if err := h.Prepare(SlotPreview, "preview-2", "scene-2", "sha256:bbb", program, nil, nil, nil); err != nil {
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

	if err := h.Prepare(SlotPreview, "preview-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
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

// TestHost_ServingRequiresBothSceneIDAndDigestToMatch is the unit-level
// proof for the slot-identity fix backing Prepare's idempotent short-circuit
// (ADR-BLUE-012 §4.4): digest equality alone is not enough — a slot Prepared
// for one scene must never read as "Serving" a different scene just because
// the digest happens to coincide, or a caller could reuse that scene's
// running instance (and its accumulated state) for an unrelated intent.
func TestHost_ServingRequiresBothSceneIDAndDigestToMatch(t *testing.T) {
	h := NewHost()
	program := fixture(t)

	if h.Serving(SlotPreview, "scene-1", "sha256:aaa") {
		t.Fatal("expected Serving false on an empty slot")
	}

	if err := h.Prepare(SlotPreview, "preview-1", "scene-1", "sha256:aaa", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if !h.Serving(SlotPreview, "scene-1", "sha256:aaa") {
		t.Fatal("expected Serving true for the exact (sceneID, digest) just Prepared")
	}
	if h.Serving(SlotPreview, "scene-1", "sha256:zzz") {
		t.Fatal("expected Serving false: same sceneID, different digest must not match")
	}
	// The case that matters: a different scene sharing the same digest
	// must never read as "already serving".
	if h.Serving(SlotPreview, "scene-2", "sha256:aaa") {
		t.Fatal("expected Serving false: same digest, different sceneID must not match")
	}
}
