package bluehost

import (
	"errors"
	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

// SwapAuthority moves admitted actors between roles under both admission leases.
// Caller pauses and joins both bridges first. No runtime Load/Start/Step occurs.
func (h *Host) SwapAuthority() error {
	h.runtimeMu.Lock()
	defer h.runtimeMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.transitions[SlotPreview] || h.transitions[SlotOnAir] {
		return errors.New("LANE_COMMIT_BLUE_BUSY")
	}
	p, v := h.slots[SlotOnAir], h.slots[SlotPreview]
	commits := []func(){}
	for _, item := range []struct {
		e    *entry
		mode blueruntime.Mode
	}{{p, blueruntime.Preview}, {v, blueruntime.Execute}} {
		if item.e == nil || item.e.instance == nil {
			continue
		}
		commit, err := h.runtime.PrepareAuthority(item.e.instance, item.mode, NewEffectHandlers(h.effectDeps, item.mode))
		if err != nil {
			return errors.New("LANE_COMMIT_BLUE_AUTHORITY_REFUSED")
		}
		commits = append(commits, commit)
	}
	for _, commit := range commits {
		commit()
	}
	if v == nil {
		delete(h.slots, SlotOnAir)
	} else {
		h.slots[SlotOnAir] = v
	}
	if p == nil {
		delete(h.slots, SlotPreview)
	} else {
		h.slots[SlotPreview] = p
	}
	h.programAdmissionEpoch++
	h.programSuspended = v == nil
	return nil
}
