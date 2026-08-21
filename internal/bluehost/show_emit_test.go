package bluehost

import (
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

func TestDispatchShowEmitUsesCausationIdentityAndPreservesIdenticalPayloads(t *testing.T) {
	h := NewHost()
	instance := &blueruntime.InstanceHandle{}
	h.slots[SlotOnAir] = &entry{instance: instance, showEmitSeen: map[string]struct{}{}}
	var calls []showEmitCall
	h.SetShowEmitSink(func(topic string, payload any) {
		calls = append(calls, showEmitCall{topic: topic, payload: payload})
	})

	variables := func(causation string) map[string]any {
		return map[string]any{
			showEmitBag: map[string]any{
				"emit_chat": map[string]any{
					"node_id":            "emit_chat",
					"opcode":             "core.show.emit@1",
					"causation_event_id": causation,
					"correlation_id":     causation,
					"config":             map[string]any{"topic": "stream_chat_event"},
					"inputs":             map[string]any{"payload": map[string]any{"text": "same"}},
				},
			},
		}
	}

	h.dispatchShowEmit(SlotOnAir, instance, variables("platform-1"))
	h.dispatchShowEmit(SlotOnAir, instance, variables("platform-1"))
	h.dispatchShowEmit(SlotOnAir, instance, variables("platform-2"))

	if len(calls) != 2 {
		t.Fatalf("show.emit calls = %d, want 2 (same payload on two dispatches)", len(calls))
	}
	for _, call := range calls {
		if call.topic != "stream_chat_event" {
			t.Fatalf("show.emit topic = %q", call.topic)
		}
	}
}

func TestBuildShowEmitEventIsAcceptedByBlueRuntime(t *testing.T) {
	raw, err := buildShowEmitEvent("orion_show_on_air_test", "stream_chat_event", 1, map[string]any{"text": "hello"})
	if err != nil {
		t.Fatalf("buildShowEmitEvent: %v", err)
	}
	if _, err := blueruntime.ParseEvent(raw); err != nil {
		t.Fatalf("blue runtime rejected show.emit event: %v", err)
	}
}
