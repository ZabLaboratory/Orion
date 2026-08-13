package bluewire

import (
	"context"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

type fakeSteps struct {
	results []StepResult
	errs    []error
	calls   int
}

func (f *fakeSteps) Step(_ bluehost.Slot) (StepResult, error) {
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
	if steps.calls < 2 {
		t.Fatalf("expected the loop to keep stepping after an error, got %d calls", steps.calls)
	}
}
