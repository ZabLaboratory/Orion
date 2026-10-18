package protocol

import (
	"errors"
	"sync"
)

// SequenceTracker is a goroutine-safe counter that implements the
// LSDP/1.x sequencing rules :
//
//   - The first frame of a subscription establishes the baseline. Under
//     1.0 it was always seq = 1 ; under 1.1 (per spec §18.1.1) it can
//     be any value because seq is per-scene, not per-subscription.
//   - Each subsequent Delta / SceneChanged / Error increments by 1.
//   - A snapshot frame received mid-stream resets the baseline (used
//     after SceneChanged or back-pressure recovery).
//
// On the receiver side, ObserveServer detects gaps (seq > last + 1)
// and returns ErrGap so the caller can close + reconnect with a
// VERSION_GAP reason. seq <= last is silently dropped (replay).
type SequenceTracker struct {
	mu  sync.Mutex
	cur uint64
}

// ErrGap signals a missing sequence number on the server-to-client
// stream. The receiver MUST close the WebSocket and reconnect ; a
// fresh Snapshot will reset state.
var ErrGap = errors.New("protocol: sequence gap")

// ErrInvalidSeqStart is returned by ObserveServer if the first frame
// of a subscription carries seq == 0. (Under 1.0 the constraint was
// stricter — first frame must be seq == 1 ; relaxed in 1.1 to accept
// any positive value.)
var ErrInvalidSeqStart = errors.New("protocol: subscription must start at seq>=1")

// NewSequenceTracker returns a tracker with cur = 0 (no frame seen).
func NewSequenceTracker() *SequenceTracker {
	return &SequenceTracker{}
}

// NextServer returns the next outgoing seq for a server emitting a
// frame on this subscription. Use Reset() when emitting the snapshot
// after a SceneChanged.
func (s *SequenceTracker) NextServer() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur++
	return s.cur
}

// Reset rewinds the counter so the next NextServer call returns 1.
// Call this immediately before emitting the Snapshot that follows a
// SceneChanged.
func (s *SequenceTracker) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = 0
}

// Current returns the last seq emitted (0 if none yet).
func (s *SequenceTracker) Current() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// ObserveServer is the receiver-side check. Pass the seq from the
// most recently received server frame. Returns :
//
//   - nil : seq is the expected next value, advance.
//   - ErrGap : seq > last+1, caller MUST close and reconnect.
//   - nil with skip=true : seq <= last, replay — caller MUST drop frame.
//
// The first call on a fresh tracker accepts any seq >= 1 as the
// baseline (see [SequenceTracker] doc — under 1.1, scene seq is global
// so late-joining subscribers may start at seq > 1).
func (s *SequenceTracker) ObserveServer(seq uint64) (skip bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.cur == 0:
		// Fresh tracker — any seq >= 1 establishes the baseline.
		if seq < 1 {
			return false, ErrInvalidSeqStart
		}
		s.cur = seq
		return false, nil
	case seq == s.cur+1:
		s.cur = seq
		return false, nil
	case seq <= s.cur:
		return true, nil // silent replay
	default:
		return false, ErrGap
	}
}

// ObserveSnapshot resets the tracker to the seq carried by a snapshot
// frame. Use this when receiving a snapshot after a SceneChanged or
// back-pressure recovery — the seq baseline jumps to the snapshot
// value regardless of previous state.
func (s *SequenceTracker) ObserveSnapshot(seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq < 1 {
		return ErrInvalidSeqStart
	}
	s.cur = seq
	return nil
}
