package bluehost

import (
	"encoding/json"
	"testing"
)

const projectedPlatformLeaf = "__inputs.platform.twitch.channel_1.last_follow"

func buildProjectedSideEffectProgram(t *testing.T) []byte {
	t.Helper()
	execPort := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	dataPort := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": typ, "required": required}
	}
	program := map[string]any{
		"schema_version":   "blue.program.v1",
		"program_id":       "host-variable-projection",
		"compiler_version": "0.1.0",
		"runtime_abi":      "blue-runtime-abi.v1",
		"runtime_module":   map[string]any{"name": "blue-runtime-go", "version": "0.1.0"},
		"source_revision": map[string]any{
			"id": "host-variable-projection", "kind": "blueprint-version", "revision": 1,
			"digest": "sha256:3aa201ed0203ce41437936bc3d4de2f6b7f3689117eec787ad0a4b5a7eb64158",
		},
		"determinism": map[string]any{"clock": "injected", "entropy": "injected", "map_iteration": "canonical", "scheduler": "injected"},
		"errors":      map[string]any{"profile": "blue.runtime.error.v1", "unhandled": "halt"},
		"ordering": map[string]any{
			"duplicate_event": "idempotent_same_digest", "effect_completion": "explicit_fifo_event",
			"event_inbox": "fifo_by_runtime_sequence", "event_sequence_scope": "instance_per_origin",
			"exec_fanout": "ascending_edge_sequence", "sequence_gap": "reject", "timer_tie_break": "due_at_then_timer_id",
		},
		"budgets": map[string]any{
			"max_steps_per_dispatch": 16, "max_queue_depth": 16, "max_execution_ms": 1000,
			"max_pending_effects": 0, "max_effects_per_dispatch": 0, "max_timers": 0,
		},
		"types": []any{
			map[string]any{"id": "core.json", "kind": "primitive", "spec": map[string]any{"base": "json"}},
			map[string]any{"id": "core.string", "kind": "primitive", "spec": map[string]any{"base": "string"}},
		},
		"opcodes": []any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{execPort("then")}},
			map[string]any{"id": "core.event.on-tick@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{dataPort("delta_seconds", "core.json", false), execPort("then")}},
			map[string]any{"id": "core.event.on-platform-event@1", "kind": "entrypoint",
				"config": []any{dataPort("channel", "core.string", false), dataPort("event_type", "core.string", false), dataPort("platform", "core.string", false)},
				"inputs": []any{}, "outputs": []any{dataPort("payload", "core.json", false), execPort("then")}},
			map[string]any{"id": "core.show.emit@1", "kind": "control",
				"config": []any{dataPort("topic", "core.string", false)},
				"inputs": []any{dataPort("payload", "core.json", false), execPort("in")}, "outputs": []any{execPort("then")}},
			map[string]any{"id": "core.overlay-app.set@1", "kind": "control",
				"config": []any{dataPort("app_id", "core.string", false), dataPort("on_air", "core.json", false), dataPort("running", "core.json", false)},
				"inputs": []any{execPort("in")}, "outputs": []any{execPort("then")}},
		},
		"nodes": []any{
			map[string]any{"id": "event", "opcode": "core.event.on-platform-event@1", "config": map[string]any{"platform": "twitch", "channel": "channel_1", "event_type": "follow"}},
			map[string]any{"id": "event-emit", "opcode": "core.show.emit@1", "config": map[string]any{"topic": "event-topic"}},
			map[string]any{"id": "event-overlay", "opcode": "core.overlay-app.set@1", "config": map[string]any{"app_id": "event-overlay", "running": true, "on_air": true}},
			map[string]any{"id": "start", "opcode": "core.event.on-start@1", "config": map[string]any{}},
			map[string]any{"id": "start-emit", "opcode": "core.show.emit@1", "config": map[string]any{"topic": "start-topic"}},
			map[string]any{"id": "start-overlay", "opcode": "core.overlay-app.set@1", "config": map[string]any{"app_id": "start-overlay", "running": true, "on_air": true}},
			map[string]any{"id": "tick", "opcode": "core.event.on-tick@1", "config": map[string]any{}},
			map[string]any{"id": "tick-emit", "opcode": "core.show.emit@1", "config": map[string]any{"topic": "tick-topic"}},
			map[string]any{"id": "tick-overlay", "opcode": "core.overlay-app.set@1", "config": map[string]any{"app_id": "tick-overlay", "running": true, "on_air": true}},
		},
		"entrypoints": []any{
			map[string]any{"id": "platform", "kind": "platform-event", "node_id": "event", "port": "then", "leaf": projectedPlatformLeaf},
			map[string]any{"id": "start", "kind": "start", "node_id": "start", "port": "then"},
			map[string]any{"id": "tick", "kind": "tick", "node_id": "tick", "port": "then"},
		},
		"exec_edges": []any{
			map[string]any{"from_node": "event", "from_port": "then", "to_node": "event-emit", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "event-emit", "from_port": "then", "to_node": "event-overlay", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "start", "from_port": "then", "to_node": "start-emit", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "start-emit", "from_port": "then", "to_node": "start-overlay", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "tick", "from_port": "then", "to_node": "tick-emit", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "tick-emit", "from_port": "then", "to_node": "tick-overlay", "to_port": "in", "sequence": 0},
		},
		"data_edges": []any{map[string]any{"from_node": "event", "from_port": "payload", "to_node": "event-emit", "to_port": "payload"}},
		"data_literals": []any{
			map[string]any{"node_id": "start-emit", "port": "payload", "value": map[string]any{"source": "start"}},
			map[string]any{"node_id": "tick-emit", "port": "payload", "value": map[string]any{"source": "tick"}},
		},
		"effects": []any{}, "requires": []any{}, "topics": []any{}, "timers": []any{},
		"state": map[string]any{"variables": []any{}, "outputs": []any{}},
	}
	for _, key := range []string{"opcodes", "nodes", "entrypoints"} {
		sortByID(program[key].([]any))
	}
	program["program_digest"] = digestOf(program)
	data, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal projection fixture: %v", err)
	}
	return data
}

func TestHostProjectedStepTickAndPlatformEventKeepLocalSideEffects(t *testing.T) {
	host := NewHost()
	mirror := &fakeOverlayMirror{}
	host.SetOverlayMirror(mirror)
	var topics []string
	var payloads []any
	host.SetShowEmitSink(func(topic string, payload any) {
		topics = append(topics, topic)
		payloads = append(payloads, payload)
	})
	if err := host.Prepare(SlotOnAir, "projection-instance", "projection-scene", "sha256:projection", buildProjectedSideEffectProgram(t), nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = host.Release(SlotOnAir, "test-cleanup") })

	started, err := host.StepProjected(SlotOnAir)
	if err != nil {
		t.Fatalf("StepProjected start: %v", err)
	}
	assertProjectedSideEffectKeys(t, started.Variables)
	if len(topics) != 1 || topics[0] != "start-topic" || len(mirror.calls) != 1 || mirror.calls[0].appID != "start-overlay" {
		t.Fatalf("StepProjected side effects: topics=%v overlays=%#v", topics, mirror.calls)
	}

	tick, err := host.TickProjected(SlotOnAir, 0.25)
	if err != nil {
		t.Fatalf("TickProjected: %v", err)
	}
	assertProjectedSideEffectKeys(t, tick.Variables)
	if len(topics) != 2 || topics[1] != "tick-topic" {
		t.Fatalf("TickProjected show.emit dispatches = %v, want start-topic then tick-topic", topics)
	}
	if got, ok := payloads[1].(map[string]any); !ok || got["source"] != "tick" {
		t.Fatalf("TickProjected show.emit payload = %#v", payloads[1])
	}
	if len(mirror.calls) != 2 || mirror.calls[1].appID != "tick-overlay" {
		t.Fatalf("TickProjected overlay dispatches = %#v, want start-overlay then tick-overlay", mirror.calls)
	}

	eventPayload := map[string]any{"text": "hello"}
	event, err := host.WritePlatformEventProjected(SlotOnAir, projectedPlatformLeaf, eventPayload)
	if err != nil {
		t.Fatalf("WritePlatformEventProjected: %v", err)
	}
	assertProjectedSideEffectKeys(t, event.Variables)
	if len(topics) != 3 || topics[2] != "event-topic" {
		t.Fatalf("platform event show.emit dispatches = %v, want start-topic, tick-topic, then event-topic", topics)
	}
	if got, ok := payloads[2].(map[string]any); !ok || got["text"] != "hello" {
		t.Fatalf("platform event show.emit payload = %#v", payloads[2])
	}
	if len(mirror.calls) != 3 || mirror.calls[2].appID != "event-overlay" {
		t.Fatalf("platform event overlay dispatches = %#v, want start-overlay, tick-overlay, then event-overlay", mirror.calls)
	}
}

func assertProjectedSideEffectKeys(t *testing.T, variables map[string]any) {
	t.Helper()
	if len(variables) != 2 {
		t.Fatalf("projected host variables = %#v, want exactly the two side-effect bags", variables)
	}
	if _, ok := variables[showEmitBag]; !ok {
		t.Fatalf("projected host variables omit %s: %#v", showEmitBag, variables)
	}
	if _, ok := variables[overlayAppSetBag]; !ok {
		t.Fatalf("projected host variables omit %s: %#v", overlayAppSetBag, variables)
	}
}
