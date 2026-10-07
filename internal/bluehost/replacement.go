package bluehost

import (
	"errors"
	"fmt"
	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

var ErrSceneChanging = errors.New("bluehost: scene transition in progress")

// Replacement retains the exact previous instance, including await/clock state.
// Preparing never steps the candidate and never replays the previous on-start.
type Replacement struct {
	host              *Host
	slot              Slot
	previous, next    *entry
	committed, closed bool
}

func (h *Host) StageReplacement(slot Slot, instanceID, sceneID, digest string, program []byte,
	providers []map[string]any, policy blueruntime.CapabilityPolicy, handlers map[string]blueruntime.EffectFunc) (*Replacement, error) {
	h.runtimeMu.Lock()
	defer h.runtimeMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.transitions == nil {
		h.transitions = map[Slot]bool{}
	}
	if h.transitions[slot] {
		return nil, ErrSceneChanging
	}
	candidate := &entry{sceneID: sceneID, digest: digest}
	if len(program) > 0 {
		handle, err := h.loadProgram(program)
		if err != nil {
			return nil, err
		}
		mode := blueruntime.Preview
		if slot == SlotOnAir {
			mode = blueruntime.Execute
		}
		instance, err := h.runtime.Start(handle, blueruntime.StartOptions{InstanceID: instanceID, Mode: mode, Providers: providers, Policy: policy, EffectHandlers: handlers})
		if err != nil {
			return nil, err
		}
		metadata := h.metadataForProgram(program)
		candidate.instance = instance
		candidate.showEmitOrigin = showEmitOrigin(instanceID, slot)
		candidate.awaitTypes = metadata.awaitTypes
		candidate.triggers = metadata.triggers
		candidate.awaits = metadata.awaits
	}
	h.transitions[slot] = true
	return &Replacement{host: h, slot: slot, previous: h.slots[slot], next: candidate}, nil
}
func (r *Replacement) Commit() error {
	h := r.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.closed || r.committed || h.slots[r.slot] != r.previous {
		return fmt.Errorf("bluehost: stale replacement")
	}
	h.slots[r.slot] = r.next
	r.committed = true
	return nil
}
func (r *Replacement) Rollback() error { return r.finish(false) }
func (r *Replacement) Finalize() error { return r.finish(true) }
func (r *Replacement) finish(commit bool) error {
	h := r.host
	h.runtimeMu.Lock()
	defer h.runtimeMu.Unlock()
	h.mu.Lock()
	if r.closed {
		h.mu.Unlock()
		return nil
	}
	if commit && !r.committed {
		h.mu.Unlock()
		return errors.New("bluehost: replacement not committed")
	}
	if r.committed && h.slots[r.slot] != r.next {
		h.mu.Unlock()
		return errors.New("bluehost: replacement superseded")
	}
	stopped := r.previous
	if !commit {
		stopped = r.next
		if r.previous == nil {
			delete(h.slots, r.slot)
		} else {
			h.slots[r.slot] = r.previous
		}
	}
	delete(h.transitions, r.slot)
	r.closed = true
	h.mu.Unlock()
	if stopped != nil && stopped.instance != nil {
		return h.runtime.Stop(stopped.instance, "scene-transition")
	}
	return nil
}

func (h *Host) executionBlocked(slot Slot, instance *blueruntime.InstanceHandle) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.slots[slot]
	return h.transitions[slot] || e == nil || e.instance != instance
}
