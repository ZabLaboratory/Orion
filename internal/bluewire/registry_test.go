package bluewire

import (
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
)

func TestRegistry_StartThenStop(t *testing.T) {
	reg := NewRegistry()
	steps := &fakeSteps{results: []StepResult{{RuntimeSequence: 1, Outputs: map[string]any{"a": "1"}}}}
	mirror := &fakeMirror{}
	bridge := &Bridge{steps: steps, slot: bluehost.SlotPreview, mirror: mirror, target: blueproject.TargetPreview}

	reg.Start(bluehost.SlotPreview, bridge, 5*time.Millisecond, nil)
	if !reg.Running(bluehost.SlotPreview) {
		t.Fatal("expected slot to be running after Start")
	}

	time.Sleep(20 * time.Millisecond)
	reg.Stop(bluehost.SlotPreview)
	if reg.Running(bluehost.SlotPreview) {
		t.Fatal("expected slot to be stopped after Stop")
	}

	// Same in-flight-tick race as the swap test below: give any step
	// already dequeued from the ticker before cancellation a moment to
	// finish, then assert no further growth.
	time.Sleep(10 * time.Millisecond)
	callsAtStop := steps.calls
	time.Sleep(20 * time.Millisecond)
	if steps.calls != callsAtStop {
		t.Fatalf("expected no further steps after Stop, calls went from %d to %d", callsAtStop, steps.calls)
	}
}

func TestRegistry_StartReplacesPreviousBridgeForSameSlot(t *testing.T) {
	reg := NewRegistry()
	oldSteps := &fakeSteps{results: []StepResult{{RuntimeSequence: 1}}}
	oldMirror := &fakeMirror{}
	oldBridge := &Bridge{steps: oldSteps, slot: bluehost.SlotOnAir, mirror: oldMirror, target: blueproject.TargetProgram}
	reg.Start(bluehost.SlotOnAir, oldBridge, 5*time.Millisecond, nil)
	time.Sleep(15 * time.Millisecond)

	newSteps := &fakeSteps{results: []StepResult{{RuntimeSequence: 1}}}
	newMirror := &fakeMirror{}
	newBridge := &Bridge{steps: newSteps, slot: bluehost.SlotOnAir, mirror: newMirror, target: blueproject.TargetProgram}
	reg.Start(bluehost.SlotOnAir, newBridge, 5*time.Millisecond, nil)

	// A ticker fire already in flight when Stop cancels the old bridge's
	// context can land one more step before the goroutine observes
	// cancellation (Run's select is non-deterministic when both cases are
	// ready) — allow that single race window, then assert no further growth.
	time.Sleep(10 * time.Millisecond)
	oldCallsAtSwap := oldSteps.calls
	time.Sleep(20 * time.Millisecond)
	if oldSteps.calls != oldCallsAtSwap {
		t.Fatalf("expected the superseded bridge to stop stepping; calls went from %d to %d", oldCallsAtSwap, oldSteps.calls)
	}
	if newSteps.calls == 0 {
		t.Fatal("expected the replacing bridge to be stepping")
	}
	reg.Stop(bluehost.SlotOnAir)
}

func TestRegistry_StopOnEmptySlotIsNoOp(t *testing.T) {
	reg := NewRegistry()
	reg.Stop(bluehost.SlotPreview) // must not panic
	if reg.Running(bluehost.SlotPreview) {
		t.Fatal("expected slot to report not running")
	}
}

func TestRegistry_StopAll(t *testing.T) {
	reg := NewRegistry()
	previewBridge := &Bridge{steps: &fakeSteps{}, slot: bluehost.SlotPreview, mirror: &fakeMirror{}, target: blueproject.TargetPreview}
	onAirBridge := &Bridge{steps: &fakeSteps{}, slot: bluehost.SlotOnAir, mirror: &fakeMirror{}, target: blueproject.TargetProgram}
	reg.Start(bluehost.SlotPreview, previewBridge, 5*time.Millisecond, nil)
	reg.Start(bluehost.SlotOnAir, onAirBridge, 5*time.Millisecond, nil)

	reg.StopAll()
	if reg.Running(bluehost.SlotPreview) || reg.Running(bluehost.SlotOnAir) {
		t.Fatal("expected every slot to be stopped after StopAll")
	}
}
