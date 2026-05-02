// Package runtime owns the live state of every active+pushed scene
// in the show. One goroutine per scene per ADR 004 § 4. Inter-scene
// data sharing is forbidden: scenes communicate only via the adapter
// inbox writing to declared paths in each subscribed scene.
package runtime

import (
	"encoding/json"
	"sync"
)

// State is a single scene's leaf-path state. It is *not* concurrent
// safe — the scene goroutine is the only writer/reader, and the WS
// subscription layer obtains snapshots via the dedicated Snapshot()
// method which copies under a lock.
type State struct {
	mu       sync.RWMutex
	values   map[string]json.RawMessage
	dirty    map[string]bool
	sequence uint64
}

// NewState builds an empty state.
func NewState() *State {
	return &State{
		values: map[string]json.RawMessage{},
		dirty:  map[string]bool{},
	}
}

// Seed populates the state from the graph's declared defaults at
// scene activation / cold start.
func (s *State) Seed(defaults map[string]json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range defaults {
		s.values[k] = v
	}
}

// Get returns the current value at path, plus whether the path
// exists. Used by computes and snapshot building.
func (s *State) Get(path string) (json.RawMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[path]
	return v, ok
}

// Set writes a new value at path and marks it dirty if changed.
// Equal-value writes are dropped silently — protocol-level idempotency
// (ADR 002 § 6 input semantics).
func (s *State) Set(path string, value json.RawMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.values[path]; ok && rawEqual(old, value) {
		return false
	}
	s.values[path] = value
	s.dirty[path] = true
	return true
}

// MarkDirty flags the path without changing its value. Used during
// recompute when a downstream node's upstream is dirty: even if the
// computed value happens to be unchanged, the propagation walks
// dependents — the dirty bit clears once the recompute decides
// whether to emit (Set returns false on no-change), which is the
// only mechanism that prevents a needless delta.
func (s *State) MarkDirty(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty[path] = true
}

// IsDirty reports the dirty flag.
func (s *State) IsDirty(path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty[path]
}

// FlushDirty returns the set of paths dirty since the last call and
// clears the dirty bitmap. The scene loop calls this after each
// recompute to assemble the outbound delta.
func (s *State) FlushDirty() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.dirty))
	for p := range s.dirty {
		out = append(out, p)
	}
	s.dirty = map[string]bool{}
	return out
}

// Snapshot returns a deep copy of the state — used to seed new WS
// subscriptions and to collapse-replay a slow consumer.
func (s *State) Snapshot() (uint64, map[string]json.RawMessage) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]json.RawMessage, len(s.values))
	for k, v := range s.values {
		// Copy the bytes so mutations on either side don't leak.
		cp := make(json.RawMessage, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return s.sequence, out
}

// AdvanceSequence bumps the per-scene sequence counter and returns
// the new value. Called once per outbound message (Snapshot, Delta,
// SceneChanged).
func (s *State) AdvanceSequence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence++
	return s.sequence
}

// Sequence returns the current sequence without advancing.
func (s *State) Sequence() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sequence
}

// ResetSequence is used on scene_changed (ADR 002 § 7): the
// destination scene's snapshot reseeds the sequence.
func (s *State) ResetSequence() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence = 0
}

// rawEqual checks byte-equality of two RawMessage values. Both sides
// have already been canonical-normalised at compile time, so byte
// equality matches semantic equality for state writes.
func rawEqual(a, b json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
