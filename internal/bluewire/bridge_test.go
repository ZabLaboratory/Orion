package bluewire

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// fakeSteps is shared between a Bridge's own goroutine (Run/StepOnce
// calling Step) and the test goroutine reading .calls to assert on
// progress — calls must be mutex-guarded or `go test -race` (CI's
// build-test job) flags it, even though the two writers/readers never
// logically overlap by test design.
type fakeSteps struct {
	mu         sync.Mutex
	results    []StepResult
	errs       []error
	calls      int
	tickDeltas []float64
	tickSlots  []bluehost.Slot
}

func (f *fakeSteps) Step(_ bluehost.Slot) (StepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextResultLocked()
}

func (f *fakeSteps) Tick(slot bluehost.Slot, deltaSeconds float64) (StepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tickSlots = append(f.tickSlots, slot)
	f.tickDeltas = append(f.tickDeltas, deltaSeconds)
	return f.nextResultLocked()
}

func (f *fakeSteps) nextResultLocked() (StepResult, error) {
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return StepResult{}, f.errs[i]
	}
	if i < len(f.results) {
		return f.results[i], nil
	}
	return StepResult{}, nil
}

func (f *fakeSteps) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSteps) tickDeltasSnapshot() ([]bluehost.Slot, []float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	slots := append([]bluehost.Slot(nil), f.tickSlots...)
	deltas := append([]float64(nil), f.tickDeltas...)
	return slots, deltas
}

type fakeMirror struct {
	forwarded []any
}

func (m *fakeMirror) Forward(msg any) {
	m.forwarded = append(m.forwarded, msg)
}

func TestBridge_StepOnce_ForwardsDelta(t *testing.T) {
	steps := &fakeSteps{results: []StepResult{
		{RuntimeSequence: 3, Outputs: map[string]any{"title.text": "hello"}},
	}}
	mirror := &fakeMirror{}
	b := &Bridge{steps: steps, slot: bluehost.SlotPreview, mirror: mirror,
		sceneID: "scene-1", sceneDigest: "sha256:abc", instanceID: "inst-1", target: blueproject.TargetPreview}

	if err := b.StepOnce(); err != nil {
		t.Fatalf("StepOnce: %v", err)
	}
	if len(mirror.forwarded) != 1 {
		t.Fatalf("expected 1 forwarded message, got %d", len(mirror.forwarded))
	}
	delta, ok := mirror.forwarded[0].(*protocol.Delta)
	if !ok {
		t.Fatalf("expected *protocol.Delta, got %T", mirror.forwarded[0])
	}
	if delta.SceneID != "scene-1" || delta.Sequence != 3 || len(delta.Patches) != 1 {
		t.Fatalf("unexpected delta: %+v", delta)
	}
}

// TestBridge_StepOnce_LogsSuccessfulForward proves the ADR-BLUE-012 §16.1
// out-of-wire trace actually fires with the full projection identity on a
// real forward — the gap this test closes is that bridge.go previously had
// no log path at all for a successful step (only StepOnce's own error
// return was observable), so nothing outside a live WS subscriber could
// learn "this correlation_id was just projected to this slot at sequence N"
// (Refs B3-R6-16-ORION-PGM, B3-R6-17-PULSAR).
func TestBridge_StepOnce_LogsSuccessfulForward(t *testing.T) {
	steps := &fakeSteps{results: []StepResult{
		{RuntimeSequence: 5, Outputs: map[string]any{"title.text": "hello"}},
	}}
	mirror := &fakeMirror{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	b := &Bridge{
		steps: steps, slot: bluehost.SlotOnAir, mirror: mirror,
		sceneID: "scene-1", sceneDigest: "sha256:abc", instanceID: "inst-1",
		target: blueproject.TargetProgram, renderRevision: "rev-1", correlationID: "corr-1",
	}
	b.SetLogger(logger)

	if err := b.StepOnce(); err != nil {
		t.Fatalf("StepOnce: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"bluewire projection forwarded",
		"scene_id=scene-1",
		"target=program",
		"scene_digest=sha256:abc",
		"runtime_instance_id=inst-1",
		"render_revision=rev-1",
		"correlation_id=corr-1",
		"sequence=5",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected log line to contain %q, got: %s", want, out)
		}
	}
}

// TestBridge_StepOnce_NoLogWithoutLogger proves SetLogger is genuinely
// optional (nil-safe, matching SetWSMetrics/SetExecMetrics elsewhere) — a
// bridge that never had SetLogger called behaves exactly as before this
// change: no panic, no log, forward unaffected.
func TestBridge_StepOnce_NoLogWithoutLogger(t *testing.T) {
	steps := &fakeSteps{results: []StepResult{
		{RuntimeSequence: 1, Outputs: map[string]any{"a": "1"}},
	}}
	mirror := &fakeMirror{}
	b := &Bridge{steps: steps, slot: bluehost.SlotPreview, mirror: mirror, target: blueproject.TargetPreview}

	if err := b.StepOnce(); err != nil {
		t.Fatalf("StepOnce: %v", err)
	}
	if len(mirror.forwarded) != 1 {
		t.Fatalf("expected the forward to still happen with no logger installed, got %d", len(mirror.forwarded))
	}
}

// TestBridge_StepOnce_NoLogOnEmptyOrDuplicateProjection proves the trace is
// scoped to genuine forwards only: an empty-projection step (nothing to
// mirror) and an identical-projection replay (deduplicated at the wire,
// TestBridge_TickOnce_UsesInjectedClockAndDeterministicProjection already
// covers the dedup mechanism itself) must not manufacture a false forward
// trace for either case.
func TestBridge_StepOnce_NoLogOnEmptyOrDuplicateProjection(t *testing.T) {
	steps := &fakeSteps{results: []StepResult{
		{RuntimeSequence: 1, Outputs: nil}, // empty projection: recordRuntimeSequence path, no forward
	}}
	mirror := &fakeMirror{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	b := &Bridge{steps: steps, slot: bluehost.SlotOnAir, mirror: mirror, target: blueproject.TargetProgram}
	b.SetLogger(logger)

	if err := b.StepOnce(); err != nil {
		t.Fatalf("StepOnce: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no forward trace for an empty projection, got: %s", buf.String())
	}
}

// TestBridge_TickOnce_NoLogOnDeduplicatedReplay proves a replayed identical
// projection — deduplicated before ever reaching mirror.Forward — does not
// manufacture a second forward trace. Same fixture as
// TestBridge_TickOnce_UsesInjectedClockAndDeterministicProjection, with a
// logger attached to assert on the trace count directly.
func TestBridge_TickOnce_NoLogOnDeduplicatedReplay(t *testing.T) {
	ticks := &fakeSteps{results: []StepResult{
		{Outputs: map[string]any{"a": "first"}},
		{Outputs: map[string]any{"a": "first"}},
	}}
	mirror := &fakeMirror{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	b := &Bridge{
		ticks: ticks, slot: bluehost.SlotOnAir, mirror: mirror,
		sceneID: "scene-1", target: blueproject.TargetProgram,
	}
	b.SetLogger(logger)

	if err := b.TickOnce(0.1); err != nil {
		t.Fatalf("first TickOnce: %v", err)
	}
	if err := b.TickOnce(0.1); err != nil {
		t.Fatalf("duplicate TickOnce: %v", err)
	}
	if len(mirror.forwarded) != 1 {
		t.Fatalf("expected the replay to be deduplicated at the wire, got %d forwards", len(mirror.forwarded))
	}
	if got := strings.Count(buf.String(), "bluewire projection forwarded"); got != 1 {
		t.Fatalf("expected exactly 1 forward trace (not one per TickOnce call), got %d in: %s", got, buf.String())
	}
}

func TestBridge_StepOnce_EmptyProjectionIsNoOp(t *testing.T) {
	steps := &fakeSteps{results: []StepResult{{RuntimeSequence: 1, Outputs: nil}}}
	mirror := &fakeMirror{}
	b := &Bridge{steps: steps, slot: bluehost.SlotOnAir, mirror: mirror, target: blueproject.TargetProgram}

	if err := b.StepOnce(); err != nil {
		t.Fatalf("StepOnce: %v", err)
	}
	if len(mirror.forwarded) != 0 {
		t.Fatalf("expected no forwarded message, got %d", len(mirror.forwarded))
	}
}

func TestBridge_StepOnce_PropagatesError(t *testing.T) {
	steps := &fakeSteps{errs: []error{bluehost.ErrNotLoaded}}
	mirror := &fakeMirror{}
	b := &Bridge{steps: steps, slot: bluehost.SlotPreview, mirror: mirror}

	if err := b.StepOnce(); err == nil {
		t.Fatal("expected error to propagate")
	}
	if len(mirror.forwarded) != 0 {
		t.Fatalf("expected no forward on error, got %d", len(mirror.forwarded))
	}
}

func TestBridge_TickOnce_UsesInjectedClockAndDeterministicProjection(t *testing.T) {
	ticks := &fakeSteps{results: []StepResult{
		{Outputs: map[string]any{"z": "last", "a": "first"}},
		{Outputs: map[string]any{"z": "last", "a": "first"}},
		{Outputs: map[string]any{"z": "next", "a": "first"}},
	}}
	mirror := &fakeMirror{}
	b := &Bridge{
		ticks:          ticks,
		slot:           bluehost.SlotOnAir,
		mirror:         mirror,
		sceneID:        "scene-1",
		sceneDigest:    "sha256:scene",
		instanceID:     "instance-1",
		target:         blueproject.TargetProgram,
		renderRevision: "revision-1",
		correlationID:  "correlation-1",
	}

	if err := b.TickOnce(0.25); err != nil {
		t.Fatalf("first TickOnce: %v", err)
	}
	slots, deltas := ticks.tickDeltasSnapshot()
	if len(deltas) != 1 || deltas[0] != 0.25 || len(slots) != 1 || slots[0] != bluehost.SlotOnAir {
		t.Fatalf("unexpected injected tick: slots=%v deltas=%v", slots, deltas)
	}
	if len(mirror.forwarded) != 1 {
		t.Fatalf("expected one forwarded projection, got %d", len(mirror.forwarded))
	}
	first := mirror.forwarded[0].(*protocol.Delta)
	if first.Sequence != 1 || first.SchemaVersion != "orion.blue-solar-projection.v1" ||
		first.SceneDigest != "sha256:scene" || first.RuntimeInstanceID != "instance-1" ||
		first.Target != string(blueproject.TargetProgram) || first.RenderRevision != "revision-1" ||
		first.CorrelationID != "correlation-1" {
		t.Fatalf("projection metadata was not preserved: %+v", first)
	}
	if len(first.Patches) != 2 || first.Patches[0].Path != "a" || first.Patches[1].Path != "z" {
		t.Fatalf("projection patches are not deterministic: %+v", first.Patches)
	}

	if err := b.TickOnce(0.25); err != nil {
		t.Fatalf("duplicate TickOnce: %v", err)
	}
	if len(mirror.forwarded) != 1 {
		t.Fatalf("identical projection should be deduplicated, got %d forwards", len(mirror.forwarded))
	}

	if err := b.TickOnce(0.25); err != nil {
		t.Fatalf("changed TickOnce: %v", err)
	}
	if len(mirror.forwarded) != 2 {
		t.Fatalf("changed projection should be forwarded, got %d forwards", len(mirror.forwarded))
	}
	second := mirror.forwarded[1].(*protocol.Delta)
	if second.Sequence != 2 || string(second.Patches[1].Value) != `"next"` {
		t.Fatalf("expected monotone changed projection, got %+v", second)
	}
}

func TestBridge_StepOnce_RejectsStaleNonIdenticalRuntimeSequence(t *testing.T) {
	steps := &fakeSteps{results: []StepResult{
		{RuntimeSequence: 7, Outputs: map[string]any{"value": "first"}},
		{RuntimeSequence: 7, Outputs: map[string]any{"value": "second"}},
	}}
	mirror := &fakeMirror{}
	b := &Bridge{steps: steps, slot: bluehost.SlotOnAir, mirror: mirror, target: blueproject.TargetProgram}

	if err := b.StepOnce(); err != nil {
		t.Fatalf("first StepOnce: %v", err)
	}
	err := b.StepOnce()
	var stale *SequenceStaleError
	if !errors.As(err, &stale) {
		t.Fatalf("expected SequenceStaleError, got %v", err)
	}
	if stale.Received != 7 || stale.Last != 7 {
		t.Fatalf("unexpected stale sequence: %+v", stale)
	}
	if len(mirror.forwarded) != 1 {
		t.Fatalf("stale projection must not be forwarded, got %d", len(mirror.forwarded))
	}
}

func TestNewBridge_TickOnceUsesHostScheduler(t *testing.T) {
	program, err := os.ReadFile("../bluespike/testdata/01-minimal.program.json")
	if err != nil {
		t.Fatalf("read minimal program: %v", err)
	}
	host := bluehost.NewHost()
	if err := host.Prepare(bluehost.SlotOnAir, "instance-1", "scene-1", "sha256:scene", program, nil, nil, nil); err != nil {
		t.Fatalf("Host.Prepare: %v", err)
	}
	t.Cleanup(func() { _ = host.Release(bluehost.SlotOnAir, "test-cleanup") })
	bridge := NewBridge(host, bluehost.SlotOnAir, &fakeMirror{}, "scene-1", "sha256:scene", "instance-1", blueproject.TargetProgram, "rev-1", "corr-1")

	// Host.Tick rejects a negative injected delta before touching the runtime;
	// Host.Step would accept this call. This keeps the production wiring test
	// independent of a program containing an on-tick entrypoint.
	if err := bridge.TickOnce(-1); err == nil {
		t.Fatal("expected NewBridge.TickOnce to call Host.Tick and reject a negative delta")
	}
}

func TestBridge_Run_StepsUntilCancelled(t *testing.T) {
	steps := &fakeSteps{results: []StepResult{
		{RuntimeSequence: 1, Outputs: map[string]any{"a": "1"}},
		{RuntimeSequence: 2, Outputs: map[string]any{"a": "2"}},
		{RuntimeSequence: 3, Outputs: map[string]any{"a": "3"}},
	}}
	mirror := &fakeMirror{}
	b := &Bridge{steps: steps, slot: bluehost.SlotPreview, mirror: mirror, target: blueproject.TargetPreview}

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	b.Run(ctx, 10*time.Millisecond, nil)

	if len(mirror.forwarded) < 2 {
		t.Fatalf("expected at least 2 steps forwarded, got %d", len(mirror.forwarded))
	}
}

func TestBridge_Run_ReportsErrorsButKeepsGoing(t *testing.T) {
	steps := &fakeSteps{errs: []error{bluehost.ErrNotLoaded, nil, nil}}
	mirror := &fakeMirror{}
	b := &Bridge{steps: steps, slot: bluehost.SlotPreview, mirror: mirror, target: blueproject.TargetPreview}

	var gotErrs int
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	b.Run(ctx, 10*time.Millisecond, func(error) { gotErrs++ })

	if gotErrs == 0 {
		t.Fatal("expected onError to be called at least once")
	}
	if steps.callCount() < 2 {
		t.Fatalf("expected the loop to keep stepping after an error, got %d calls", steps.callCount())
	}
}
