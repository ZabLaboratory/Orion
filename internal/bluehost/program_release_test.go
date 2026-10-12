package bluehost

import "testing"

func TestProgramAdmissionReleaseFenceAndNextPhysicalStart(t *testing.T) {
	h := NewHost()
	old, allowed := h.ProgramAdmissionToken()
	if !allowed {
		t.Fatal("initial compatibility admission refused")
	}
	h.SuspendProgramAdmissions()
	if h.ProgramAdmissionValid(old) {
		t.Fatal("in-flight admission survived release")
	}
	if _, allowed = h.ProgramAdmissionToken(); allowed {
		t.Fatal("released Program admits scenes")
	}
	if err := h.PreparePreviewStatic("preview", "version"); err != nil {
		t.Fatal(err)
	}
	if err := h.SwapAuthority(); err != nil {
		t.Fatal(err)
	}
	current, allowed := h.ProgramAdmissionToken()
	if !allowed || !h.ProgramAdmissionValid(current) || h.ProgramAdmissionValid(old) || !h.Serving(SlotOnAir, "preview", "version") {
		t.Fatal("physical start did not transfer the existing Preview")
	}
	if h.Digest(SlotPreview) != "" {
		t.Fatal("empty outgoing Program became an active Preview")
	}
}
