package providers

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

func TestBuildEvent_ParsesAsCanonicalEnvelope(t *testing.T) {
	data, err := BuildEvent(
		"evt-1", "quasar.twitch.channel-42", "quasar.twitch.chat-message", "corr-1",
		7, 1734000000000,
		map[string]any{"message": "gg", "count": json.Number("3")},
	)
	if err != nil {
		t.Fatalf("BuildEvent: %v", err)
	}
	if _, err := blueruntime.ParseEvent(data); err != nil {
		t.Fatalf("ParseEvent(BuildEvent(...)): %v", err)
	}
}

func TestBuildEvent_TamperedPayloadFailsDigest(t *testing.T) {
	data, err := BuildEvent("evt-1", "quasar.twitch.channel-42", "quasar.twitch.chat-message", "corr-1", 7, 1734000000000, map[string]any{"message": "gg"})
	if err != nil {
		t.Fatalf("BuildEvent: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	envelope["payload"] = map[string]any{"message": "tampered"}
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := blueruntime.ParseEvent(tampered); err == nil {
		t.Fatal("expected ParseEvent to reject a tampered payload")
	}
}

func platformIngressProgram(t *testing.T, leaf string) []byte {
	t.Helper()
	execPort := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	dataPort := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": typ, "required": required}
	}
	program := map[string]any{
		"schema_version":   blueruntime.ProgramSchema,
		"program_id":       "providers-platform-ingress",
		"compiler_version": blueruntime.CompilerVersion,
		"runtime_abi":      blueruntime.RuntimeABI,
		"runtime_module":   map[string]any{"name": blueruntime.RuntimeModule, "version": blueruntime.RuntimeModuleVer},
		"source_revision": map[string]any{
			"id": "providers-platform-ingress", "kind": "blueprint-version", "revision": json.Number("1"),
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
			"max_steps_per_dispatch": json.Number("16"), "max_queue_depth": json.Number("16"), "max_execution_ms": json.Number("1000"),
			"max_pending_effects": json.Number("0"), "max_effects_per_dispatch": json.Number("0"), "max_timers": json.Number("0"),
		},
		"types": []any{
			map[string]any{"id": "core.json", "kind": "primitive", "spec": map[string]any{"base": "json"}},
			map[string]any{"id": "core.string", "kind": "primitive", "spec": map[string]any{"base": "string"}},
		},
		"opcodes": []any{
			map[string]any{"id": "core.event.on-platform-event@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{dataPort("payload", "core.json", false), execPort("then")}},
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{execPort("then")}},
			map[string]any{
				"id": "core.variable.set@1", "kind": "pure",
				"config":  []any{dataPort("variable", "core.string", true)},
				"inputs":  []any{dataPort("value", "core.json", true), execPort("in")},
				"outputs": []any{dataPort("value", "core.json", false), execPort("then")},
			},
		},
		"nodes": []any{
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "platform-entry", "opcode": "core.event.on-platform-event@1", "config": map[string]any{}},
			map[string]any{"id": "start", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		"entrypoints": []any{
			map[string]any{"id": "platform", "kind": "platform-event", "node_id": "platform-entry", "port": "then", "leaf": leaf},
			map[string]any{"id": "start", "kind": "start", "node_id": "start", "port": "then"},
		},
		"exec_edges": []any{
			map[string]any{"from_node": "platform-entry", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": json.Number("0")},
		},
		"data_edges": []any{
			map[string]any{"from_node": "platform-entry", "from_port": "payload", "to_node": "mark", "to_port": "value"},
		},
		"data_literals": []any{},
		"effects":       []any{},
		"requires":      []any{},
		"topics":        []any{},
		"timers":        []any{},
		"state": map[string]any{
			"variables": []any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
			"outputs":   []any{map[string]any{"name": "result", "type": "core.json"}},
		},
	}
	for _, key := range []string{"opcodes", "nodes", "entrypoints"} {
		items := program[key].([]any)
		sort.Slice(items, func(i, j int) bool {
			left, _ := items[i].(map[string]any)["id"].(string)
			right, _ := items[j].(map[string]any)["id"].(string)
			return left < right
		})
	}
	digest, err := Digest(program)
	if err != nil {
		t.Fatalf("Digest(platform program): %v", err)
	}
	program["program_digest"] = digest
	data, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal platform program: %v", err)
	}
	return data
}

func platformEvent(t *testing.T, eventID string, sequence uint64, payload map[string]any) []byte {
	t.Helper()
	data, err := BuildEvent(eventID, "quasar.twitch.channel_1", "quasar.twitch.chat", "corr-1", sequence, 1734000000000+int64(sequence), payload)
	if err != nil {
		t.Fatalf("BuildEvent(%s): %v", eventID, err)
	}
	return data
}

func blueRuntimeErrorCode(t *testing.T, err error) string {
	t.Helper()
	var runtimeErr *blueruntime.Error
	if !errors.As(err, &runtimeErr) {
		t.Fatalf("expected blue runtime error, got %T: %v", err, err)
	}
	return runtimeErr.Code
}

func TestInjectActivePlatform_UsesCanonicalLeafAndOrdering(t *testing.T) {
	const leaf = "__inputs.platform.twitch.channel_1.last_chat"
	host := bluehost.NewHost()
	program := platformIngressProgram(t, leaf)
	if err := host.Take("instance-1", "scene-1", "sha256:scene-1", program, nil, nil, nil); err != nil {
		t.Fatalf("Host.Take: %v", err)
	}
	t.Cleanup(func() { _ = host.Release(bluehost.SlotOnAir, "test-cleanup") })
	if _, err := host.Step(bluehost.SlotOnAir); err != nil {
		t.Fatalf("Host.Step(on-start): %v", err)
	}

	firstPayload := map[string]any{"message": "first"}
	firstData := platformEvent(t, "evt-1", 1, firstPayload)
	firstReceipt, err := InjectActivePlatform(host, leaf, firstData)
	if err != nil {
		t.Fatalf("InjectActivePlatform(first): %v", err)
	}
	if firstReceipt.Status != "accepted" || firstReceipt.RuntimeSequence != 1 || firstReceipt.EventID != "evt-1" {
		t.Fatalf("unexpected first receipt: %+v", firstReceipt)
	}
	step, err := host.Step(bluehost.SlotOnAir)
	if err != nil {
		t.Fatalf("Host.Step(after platform event): %v", err)
	}
	if !reflect.DeepEqual(step.Outputs["result"], firstPayload) {
		t.Fatalf("canonical leaf payload was not consumed by the program: %#v", step.Outputs["result"])
	}

	duplicate, err := InjectActivePlatform(host, leaf, firstData)
	if err != nil {
		t.Fatalf("InjectActivePlatform(duplicate): %v", err)
	}
	if duplicate != firstReceipt {
		t.Fatalf("duplicate should return the original receipt, got %+v want %+v", duplicate, firstReceipt)
	}

	gapData := platformEvent(t, "evt-3", 3, map[string]any{"message": "gap"})
	if _, err := InjectActivePlatform(host, leaf, gapData); blueRuntimeErrorCode(t, err) != "EVENT_SEQUENCE_GAP" {
		t.Fatal("expected EVENT_SEQUENCE_GAP for source sequence 3 after sequence 1")
	}

	secondData := platformEvent(t, "evt-2", 2, map[string]any{"message": "second"})
	if _, err := InjectActivePlatform(host, leaf, secondData); err != nil {
		t.Fatalf("InjectActivePlatform(second): %v", err)
	}
	conflictData := platformEvent(t, "evt-2", 2, map[string]any{"message": "conflict"})
	if _, err := InjectActivePlatform(host, leaf, conflictData); blueRuntimeErrorCode(t, err) != "EVENT_DUPLICATE_CONFLICT" {
		t.Fatal("expected EVENT_DUPLICATE_CONFLICT for reused event_id")
	}
	staleData := platformEvent(t, "evt-old", 1, map[string]any{"message": "old"})
	if _, err := InjectActivePlatform(host, leaf, staleData); blueRuntimeErrorCode(t, err) != "EVENT_SEQUENCE_CONFLICT" {
		t.Fatal("expected EVENT_SEQUENCE_CONFLICT for an obsolete source sequence")
	}

	ResetActiveIngress(host)
	resetData := platformEvent(t, "evt-reset", 1, map[string]any{"message": "new-generation"})
	resetReceipt, err := InjectActive(host, resetData)
	if err != nil {
		t.Fatalf("InjectActive(after generation reset): %v", err)
	}
	if resetReceipt.Status != "accepted" || resetReceipt.RuntimeSequence != 1 {
		t.Fatalf("expected a fresh generation receipt, got %+v", resetReceipt)
	}
}

func TestInjectActive_IsActiveOnly(t *testing.T) {
	const leaf = "__inputs.platform.twitch.channel_1.last_chat"
	host := bluehost.NewHost()
	program := platformIngressProgram(t, leaf)
	if err := host.Prepare(bluehost.SlotPreview, "preview-1", "scene-1", "sha256:preview", program, nil, nil, nil); err != nil {
		t.Fatalf("Host.Prepare(preview): %v", err)
	}
	t.Cleanup(func() { _ = host.Release(bluehost.SlotPreview, "test-cleanup") })
	if _, err := InjectActivePlatform(host, leaf, platformEvent(t, "evt-preview", 1, map[string]any{"message": "must-not-fire"})); !errors.Is(err, bluehost.ErrNotLoaded) {
		t.Fatalf("expected active-only ErrNotLoaded with preview only, got %v", err)
	}
}
