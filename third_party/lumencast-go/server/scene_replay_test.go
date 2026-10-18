package server

import (
	"sync"
	"testing"

	"github.com/Lumencast/lumencast-go/protocol"
)

func TestSceneSubscribeWithResume_ReturnsReplayMetadata(t *testing.T) {
	scene := newScene("main")
	if err := scene.Set(map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	md := &protocol.ProjectionMetadata{
		SchemaVersion:     "schema-v1",
		SceneDigest:       "digest-abc",
		RuntimeInstanceID: "runtime-xyz",
		Target:            "preview",
		RenderRevision:    "rev-123",
		CorrelationID:     "corr-456",
	}
	if err := scene.EmitWithCauseAndMetadata(map[string]any{"x": 2}, &protocol.Cause{Source: "svc"}, md); err != nil {
		t.Fatal(err)
	}

	// Since sequence 1 should be covered by buffer and replay the last delta.
	sub, snap, replay := scene.subscribeWithResume(64, true, true, 1)
	if sub == nil {
		t.Fatal("expected subscription")
	}
	if snap != nil {
		t.Fatalf("did not expect snapshot when replay is available")
	}
	if len(replay) != 1 {
		t.Fatalf("wanted 1 replay record, got %d", len(replay))
	}
	rec := replay[0]
	if rec.projectionMetadata == nil {
		t.Fatal("replay record metadata should be present")
	}
	if rec.projectionMetadata.SchemaVersion != "schema-v1" ||
		rec.projectionMetadata.SceneDigest != "digest-abc" ||
		rec.projectionMetadata.RuntimeInstanceID != "runtime-xyz" ||
		rec.projectionMetadata.Target != "preview" ||
		rec.projectionMetadata.RenderRevision != "rev-123" ||
		rec.projectionMetadata.CorrelationID != "corr-456" {
		t.Fatalf("unexpected replay metadata: %+v", rec.projectionMetadata)
	}
}

func TestSceneBackpressure_MarksSubscriptionStale(t *testing.T) {
	scene := newScene("main")
	if err := scene.Set(map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}

	// One-slot buffer means the second emission is back-pressured quickly.
	sub, snap, records := scene.subscribeWithResume(1, true, true, 0)
	if sub == nil || snap == nil || records != nil {
		t.Fatal("unexpected subscribe state for first connection without replay")
	}

	if err := scene.Emit(map[string]any{"x": 2}); err != nil {
		t.Fatal(err)
	}
	if err := scene.Emit(map[string]any{"x": 3}); err != nil {
		t.Fatal(err)
	}

	if !sub.IsStale() {
		t.Fatal("expected subscription to be marked stale under backpressure")
	}
}

// TestSceneCollapse_SnapshotCarriesLastKnownMetadata forces the real
// collapse path in sendDelta (server/scene.go, the `default:` branch of
// the back-pressure select) under genuine concurrent load, and asserts
// that any Snapshot actually delivered through it carries the scene's
// last known projection metadata. It does not construct a Snapshot by
// hand : it drives the one-slot subscriber buffer to saturation with a
// burst of real Emit calls racing a real draining goroutine, so the
// fallback branch fires for real.
func TestSceneCollapse_SnapshotCarriesLastKnownMetadata(t *testing.T) {
	scene := newScene("main")
	if err := scene.Set(map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}

	md := &protocol.ProjectionMetadata{
		SchemaVersion:     "schema-v1",
		SceneDigest:       "digest-abc",
		RuntimeInstanceID: "runtime-xyz",
		Target:            "preview",
		RenderRevision:    "rev-123",
		CorrelationID:     "corr-456",
	}
	// Establish the scene's last-known identity via a real metadata
	// emit before the subscriber attaches.
	if err := scene.EmitWithCauseAndMetadata(map[string]any{"x": 2}, nil, md); err != nil {
		t.Fatal(err)
	}

	// buffer=1 : the smallest possible window, so ordinary Emit bursts
	// saturate it almost immediately.
	sub, snap, _ := scene.subscribeWithResume(1, true, true, 0)
	if snap == nil {
		t.Fatal("expected initial snapshot")
	}
	if snap.SchemaVersion != "schema-v1" || snap.CorrelationID != "corr-456" {
		t.Fatalf("initial snapshot should already carry last known metadata: %+v", snap)
	}

	var (
		mu        sync.Mutex
		snapshots []*protocol.Snapshot
	)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for v := range sub.out {
			if s, ok := v.(*protocol.Snapshot); ok {
				mu.Lock()
				snapshots = append(snapshots, s)
				mu.Unlock()
			}
		}
	}()

	// Hammer the scene with metadata-less emits from several goroutines
	// while the drain goroutine above races to keep the single-slot
	// buffer empty. Under real concurrency this reliably drives
	// sendDelta into its `default:` collapse branch many times, and
	// occasionally frees a slot between the delta send attempt and the
	// snapshot fallback send — the exact race the production code is
	// built to tolerate.
	const goroutines = 8
	const roundsPerGoroutine = 4000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < roundsPerGoroutine; i++ {
				if err := scene.Emit(map[string]any{"x": g*roundsPerGoroutine + i}); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	scene.unsubscribe(sub)
	<-drainDone

	mu.Lock()
	defer mu.Unlock()
	if len(snapshots) == 0 {
		t.Fatal("collapse path never delivered a snapshot through sub.out — buffer never saturated under load")
	}
	for _, s := range snapshots {
		if s.SchemaVersion != "schema-v1" ||
			s.SceneDigest != "digest-abc" ||
			s.RuntimeInstanceID != "runtime-xyz" ||
			s.Target != "preview" ||
			s.RenderRevision != "rev-123" ||
			s.CorrelationID != "corr-456" {
			t.Fatalf("collapsed snapshot missing/incorrect projection metadata: %+v", s)
		}
	}
}
