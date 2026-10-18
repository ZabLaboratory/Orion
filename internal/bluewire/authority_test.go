package bluewire

import (
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"testing"
	"time"
)

func TestRoleSwapRetainsBridgeAndProjectionIdentity(t *testing.T) {
	r := NewRegistry()
	b := &Bridge{steps: &fakeSteps{}, slot: bluehost.SlotPreview, mirror: &fakeMirror{}, target: blueproject.TargetPreview, instanceID: "prepared", lastRuntimeSequence: 32, lastOutputSequence: 12}
	r.Start(bluehost.SlotPreview, b, time.Hour, nil)
	finish := r.PauseRoleSwap()
	finish(true)
	defer r.StopAll()
	if r.Current(bluehost.SlotOnAir) != b || r.Current(bluehost.SlotPreview) != nil || b.slot != bluehost.SlotOnAir || b.target != blueproject.TargetProgram || b.instanceID != "prepared" || b.lastRuntimeSequence != 32 || b.lastOutputSequence != 12 {
		t.Fatal("bridge recreated")
	}
	finish = r.PauseRoleSwap()
	finish(false)
	if r.Current(bluehost.SlotOnAir) != b {
		t.Fatal("compensation lost bridge")
	}
}
