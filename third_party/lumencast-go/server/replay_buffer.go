package server

import (
	"sync"

	"github.com/Lumencast/lumencast-go/protocol"
)

// DefaultReplayBufferSize is the default capacity (in number of delta
// records) of a Scene's replay buffer. Per LSDP/1.1 §18.1, servers
// SHOULD hold at least 256 entries.
const DefaultReplayBufferSize = 256

// replayRecord is one entry in the per-scene replay buffer.
type replayRecord struct {
	seq     uint64
	patches []protocol.Patch
	cause   *protocol.Cause
	// projectionMetadata carries optional, additive metadata associated
	// with the delta emission path.
	projectionMetadata *protocol.ProjectionMetadata
}

// replayBuffer is a bounded ring of recent (seq, patches, cause)
// emissions for a scene. Records are pushed in monotonic seq order.
// Lookup is "give me everything strictly after sinceSeq".
type replayBuffer struct {
	mu      sync.RWMutex
	records []replayRecord
	head    int // next write index
	cap     int
	size    int // number of valid entries
}

// newReplayBuffer returns a buffer of the given capacity. cap < 1
// silently coerces to DefaultReplayBufferSize.
func newReplayBuffer(cap int) *replayBuffer {
	if cap < 1 {
		cap = DefaultReplayBufferSize
	}
	return &replayBuffer{
		records: make([]replayRecord, cap),
		cap:     cap,
	}
}

// push records a new emission. Monotonic seq is the caller's
// responsibility ; the buffer does not enforce it.
func (b *replayBuffer) push(seq uint64, patches []protocol.Patch, cause *protocol.Cause, metadata *protocol.ProjectionMetadata) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Keep a defensive copy so callers mutating the incoming metadata
	// after emit cannot race with replay readers.
	var copiedMetadata *protocol.ProjectionMetadata
	if metadata != nil {
		c := *metadata
		copiedMetadata = &c
	}
	b.records[b.head] = replayRecord{
		seq:                seq,
		patches:            patches,
		cause:              cause,
		projectionMetadata: copiedMetadata,
	}
	b.head = (b.head + 1) % b.cap
	if b.size < b.cap {
		b.size++
	}
}

// since returns every record with seq > sinceSeq, in monotonic order.
// The second return value reports whether the buffer's coverage is
// sufficient — false when sinceSeq is older than the buffer's earliest
// retained entry, which is the signal to fall back to a fresh snapshot
// (LSDP/1.1 §18.1).
func (b *replayBuffer) since(sinceSeq uint64) ([]replayRecord, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.size == 0 {
		// Empty buffer — nothing to replay. The caller decides whether
		// this is "all caught up" (sinceSeq == current) or a miss.
		return nil, true
	}
	// Find the earliest retained seq.
	tail := (b.head - b.size + b.cap) % b.cap
	earliest := b.records[tail].seq
	// If the requested resume point is older than what we have, the
	// caller MUST fall back to a fresh snapshot.
	if sinceSeq+1 < earliest {
		return nil, false
	}
	out := make([]replayRecord, 0, b.size)
	for i := 0; i < b.size; i++ {
		idx := (tail + i) % b.cap
		r := b.records[idx]
		if r.seq > sinceSeq {
			out = append(out, r)
		}
	}
	return out, true
}

// reset clears the buffer. Used on scene_changed.
func (b *replayBuffer) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.head = 0
	b.size = 0
	// Note: we leave the underlying slice intact ; old entries become
	// unreachable on the next push wrap and GC reclaims their patch
	// slices when the records overwrite.
}

// length returns the number of retained entries (for tests / metrics).
func (b *replayBuffer) length() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.size
}
