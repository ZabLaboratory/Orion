package server

import (
	"fmt"
	"testing"

	"github.com/Lumencast/lumencast-go/protocol"
)

func TestReplayBuffer_PushAndSince(t *testing.T) {
	b := newReplayBuffer(4)
	for i := uint64(1); i <= 3; i++ {
		b.push(
			i,
			[]protocol.Patch{{Path: "x"}},
			nil,
			&protocol.ProjectionMetadata{
				SchemaVersion: fmt.Sprintf("schema-%02d", i),
			},
		)
	}
	if got := b.length(); got != 3 {
		t.Fatalf("length: got %d want 3", got)
	}
	recs, ok := b.since(1)
	if !ok {
		t.Fatal("since(1): want covered=true")
	}
	if len(recs) != 2 || recs[0].seq != 2 || recs[1].seq != 3 {
		t.Fatalf("since(1): got %+v", recs)
	}
	if recs[0].projectionMetadata == nil || recs[1].projectionMetadata == nil {
		t.Fatal("expected metadata to be preserved")
	}
	if recs[0].projectionMetadata.SchemaVersion != "schema-02" || recs[1].projectionMetadata.SchemaVersion != "schema-03" {
		t.Fatalf("unexpected metadata: %+v %+v", recs[0].projectionMetadata, recs[1].projectionMetadata)
	}
}

func TestReplayBuffer_PreservationIsDefensiveCopy(t *testing.T) {
	b := newReplayBuffer(4)
	md := &protocol.ProjectionMetadata{
		SchemaVersion: "schema-01",
		SceneDigest:   "digest",
	}
	b.push(1, nil, nil, md)
	md.SchemaVersion = "changed"

	recs, ok := b.since(0)
	if !ok {
		t.Fatal("since(0): want covered=true")
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0].projectionMetadata == nil {
		t.Fatal("metadata is nil after push")
	}
	if recs[0].projectionMetadata.SchemaVersion != "schema-01" {
		t.Fatalf("metadata should be copied defensively, got %s", recs[0].projectionMetadata.SchemaVersion)
	}
}

func TestReplayBuffer_RingWraparound(t *testing.T) {
	b := newReplayBuffer(4)
	for i := uint64(1); i <= 10; i++ {
		b.push(i, nil, nil, nil)
	}
	// Buffer holds the last 4 (seq 7..10). Earliest is 7.
	if got := b.length(); got != 4 {
		t.Fatalf("length: got %d want 4", got)
	}
	// since(6) → covered=true (we have 7..10)
	recs, ok := b.since(6)
	if !ok || len(recs) != 4 {
		t.Fatalf("since(6) covered=%v len=%d (want 4)", ok, len(recs))
	}
	if recs[0].seq != 7 || recs[3].seq != 10 {
		t.Fatalf("since(6) seqs: %d..%d", recs[0].seq, recs[3].seq)
	}
}

func TestReplayBuffer_GapNotCovered(t *testing.T) {
	b := newReplayBuffer(4)
	for i := uint64(1); i <= 10; i++ {
		b.push(i, nil, nil, nil)
	}
	// since(2) → not covered (we only have 7..10 ; 2 is too old)
	if _, ok := b.since(2); ok {
		t.Fatal("since(2): want covered=false (buffer earliest is 7)")
	}
}

func TestReplayBuffer_AllCaughtUp(t *testing.T) {
	b := newReplayBuffer(4)
	for i := uint64(1); i <= 3; i++ {
		b.push(i, nil, nil, nil)
	}
	recs, ok := b.since(3)
	if !ok || len(recs) != 0 {
		t.Fatalf("since(3) (caught up): covered=%v len=%d (want covered=true len=0)", ok, len(recs))
	}
}

func TestReplayBuffer_Reset(t *testing.T) {
	b := newReplayBuffer(4)
	b.push(1, nil, nil, nil)
	b.reset()
	if got := b.length(); got != 0 {
		t.Fatalf("after reset: length=%d", got)
	}
	if _, ok := b.since(0); !ok {
		t.Fatal("since(0) on empty buffer: want covered=true (nothing to replay)")
	}
}

func TestReplayBuffer_EmptyBufferAlwaysCovered(t *testing.T) {
	b := newReplayBuffer(4)
	if _, ok := b.since(99); !ok {
		t.Fatal("empty buffer: want covered=true regardless of sinceSeq")
	}
}
