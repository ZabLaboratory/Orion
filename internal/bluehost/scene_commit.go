package bluehost

import (
	"context"
	"errors"
)

// ProgramAdmissionToken fences work started before release or a physical Take.
// A released Program can only be enabled by a confirmed physical role swap.
func (h *Host) ProgramAdmissionToken() (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.programAdmissionEpoch, !h.programSuspended
}
func (h *Host) ProgramAdmissionValid(epoch uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.programSuspended && h.programAdmissionEpoch == epoch
}
func (h *Host) SuspendProgramAdmissions() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.programAdmissionEpoch++
	h.programSuspended = true
}

// AcquireSceneCommit serializes all admission callers for the same host lane.
// It is separate from runtimeMu: callbacks can execute Blue and publish LSDP
// while holding this admission lease. Different lanes retain independent leases.
func (h *Host) AcquireSceneCommit(ctx context.Context, slot Slot) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gate, ok := h.sceneCommits[slot]
	if !ok {
		return nil, errors.New("UNKNOWN_SCENE_SLOT")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return nil, err
		}
		return func() { <-gate }, nil
	}
}
