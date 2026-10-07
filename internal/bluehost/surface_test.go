package bluehost

import (
	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"testing"
)

func TestShowEventRouterBoundsReentrantCycles(t *testing.T) {
	var router ShowEmitSink
	count, overflow := 0, 0
	router = NewShowEventRouter(func(topic string, payload any) { count++; router(topic, payload) }, func() { overflow++ })
	router("cycle", true)
	if count != 1024 || overflow != 1 {
		t.Fatalf("count=%d overflow=%d", count, overflow)
	}
	// A failed cascade does not poison subsequent unrelated delivery.
	count = 0
	router("again", false)
	if count != 1024 || overflow != 2 {
		t.Fatalf("router did not recover")
	}
}
func TestLocalInvocationsRouteMutationsToTheirOwnSlot(t *testing.T) {
	host := NewHost()
	var slots []Slot
	host.SetSceneMutationSink(func(slot Slot, operations []map[string]any) error { slots = append(slots, slot); return nil })
	for _, slot := range []Slot{SlotPreview, SlotOnAir} {
		result := host.runLocalInvocation(slot, map[string]any{"capability": "core.lsml", "operation": "mutate", "request": map[string]any{"operations": []any{map[string]any{"op": "add", "path": "/layout/children/-", "value": map[string]any{"kind": "frame"}}}}})
		if result.Err != "" {
			t.Fatal(result.Err)
		}
	}
	if len(slots) != 2 || slots[0] != SlotPreview || slots[1] != SlotOnAir {
		t.Fatalf("crossed slots: %v", slots)
	}
	result := host.runLocalInvocation(SlotPreview, map[string]any{"capability": "zabcam.slots", "operation": "release", "request": map[string]any{"slot_ref": "one"}})
	if result.Err != "PREVIEW_WRITE_FORBIDDEN" {
		t.Fatalf("preview camera write admitted: %v", result)
	}
}
func TestAnimationDispatchReplaysOnlyNewCausation(t *testing.T) {
	host := NewHost()
	instance := &blueruntime.InstanceHandle{}
	host.slots[SlotPreview] = &entry{instance: instance}
	count := 0
	host.SetSceneMutationSink(func(slot Slot, operations []map[string]any) error {
		if slot != SlotPreview {
			t.Fatal("wrong slot")
		}
		count++
		return nil
	})
	record := map[string]any{"causation_event_id": "one", "config": map[string]any{"animation_id": "reveal"}, "inputs": map[string]any{}}
	variables := map[string]any{animationBag: map[string]any{"play": record}}
	host.dispatchAnimation(SlotPreview, instance, variables)
	host.dispatchAnimation(SlotPreview, instance, variables)
	record["causation_event_id"] = "two"
	host.dispatchAnimation(SlotPreview, instance, variables)
	if count != 2 {
		t.Fatalf("animation count=%d", count)
	}
}
