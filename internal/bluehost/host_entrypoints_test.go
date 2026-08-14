package bluehost

import (
	"encoding/json"
	"sort"
	"testing"
)

// buildEntrypointProgram assembles a minimal blue.program.v1 with a SECOND
// entrypoint of the given kind (start remains present so Prepare/Take's
// implicit first Step still succeeds), feeding straight into a
// `core.variable.set@1` node writing extraDataPort's implicit binding (the
// entrypoint's own carried data-out, e.g. `delta_seconds`/`payload`) into
// the declared state output "result" — proving the entrypoint fired.
func buildEntrypointProgram(t *testing.T, kind, entryOpcode, dataOutPort string, extra map[string]any) []byte {
	t.Helper()
	execPort := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	dataPort := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": typ, "required": required}
	}
	// sortedComposite(kind,name) order: data ports before exec ports.
	entryOutputs := []any{execPort("then")}
	if dataOutPort != "" {
		entryOutputs = []any{dataPort(dataOutPort, "core.json", false), execPort("then")}
	}
	entrypoint := map[string]any{"id": "arm", "kind": kind, "node_id": "tick-entry", "port": "then"}
	for k, v := range extra {
		entrypoint[k] = v
	}
	dataEdges := []any{}
	if dataOutPort != "" {
		dataEdges = []any{map[string]any{"from_node": "tick-entry", "from_port": dataOutPort, "to_node": "mark", "to_port": "value"}}
	}
	program := map[string]any{
		"schema_version":   "blue.program.v1",
		"program_id":       "fixture-entrypoint-" + kind,
		"compiler_version": "0.1.0",
		"runtime_abi":      "blue-runtime-abi.v1",
		"runtime_module":   map[string]any{"name": "blue-runtime-go", "version": "0.1.0"},
		"source_revision": map[string]any{
			"id": "fixture-entrypoint-" + kind, "kind": "blueprint-version", "revision": 1,
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
			map[string]any{"id": entryOpcode, "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": entryOutputs},
			map[string]any{
				"id": "core.variable.set@1", "kind": "pure",
				"config":  []any{dataPort("variable", "core.string", true)},
				"inputs":  []any{dataPort("value", "core.json", true), execPort("in")},
				"outputs": []any{dataPort("value", "core.json", false), execPort("then")},
			},
		},
		"nodes": []any{
			map[string]any{"id": "entry", "opcode": "core.event.on-start@1", "config": map[string]any{}},
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "tick-entry", "opcode": entryOpcode, "config": map[string]any{}},
		},
		"entrypoints": []any{
			map[string]any{"id": "start", "kind": "start", "node_id": "entry", "port": "then"},
			entrypoint,
		},
		"exec_edges": []any{
			map[string]any{"from_node": "tick-entry", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0},
		},
		"data_edges":    dataEdges,
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
	sortByID(program["opcodes"].([]any))
	sortByID(program["entrypoints"].([]any))
	program["program_digest"] = digestOf(program)
	out, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal fixture program: %v", err)
	}
	return out
}

// sortByID sorts a []any of map[string]any objects by their "id" field —
// blueruntime's program.go requires several top-level arrays (opcodes,
// entrypoints, nodes, …) in ascending "id" order (sortedObjects).
func sortByID(items []any) {
	sort.Slice(items, func(i, j int) bool {
		a, _ := items[i].(map[string]any)["id"].(string)
		b, _ := items[j].(map[string]any)["id"].(string)
		return a < b
	})
}

func TestHost_TickFiresOnTickEntrypoint(t *testing.T) {
	program := buildEntrypointProgram(t, "tick", "core.event.on-tick@1", "delta_seconds", nil)
	h := NewHost()
	if err := h.Prepare(SlotOnAir, "tick-1", "sha256:tick", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := h.Step(SlotOnAir); err != nil { // fires on-start, arms the instance
		t.Fatalf("Step (on-start): %v", err)
	}
	step, err := h.Tick(SlotOnAir, 2.5)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if v, _ := step.Outputs["result"].(json.Number); v.String() != "2.5" {
		t.Fatalf("expected result=2.5 from on-tick's delta_seconds, got %#v", step.Outputs["result"])
	}
	if _, err := h.Tick(SlotOnAir, -1); err == nil {
		t.Fatal("expected a negative delta_seconds to be rejected")
	}
}

func TestHost_CallFiresOnCallEntrypoint(t *testing.T) {
	program := buildEntrypointProgram(t, "call", "core.operator.on-call@1", "payload", nil)
	h := NewHost()
	if err := h.Prepare(SlotOnAir, "call-1", "sha256:call", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := h.Step(SlotOnAir); err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	step, err := h.Call(SlotOnAir, "arm", "hello-operator")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if v, _ := step.Outputs["result"].(string); v != "hello-operator" {
		t.Fatalf("expected result=hello-operator from on-call's payload, got %#v", step.Outputs["result"])
	}
	if _, err := h.Call(SlotOnAir, "", "x"); err == nil {
		t.Fatal("expected an empty call id to be rejected")
	}
}

func TestHost_WritePlatformEventFiresEntrypoint(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_follow"
	program := buildEntrypointProgram(t, "platform-event", "core.event.on-platform-event@1", "payload", map[string]any{"leaf": leaf})
	h := NewHost()
	if err := h.Prepare(SlotOnAir, "plat-1", "sha256:plat", program, nil, nil, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := h.Step(SlotOnAir); err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	step, err := h.WritePlatformEvent(SlotOnAir, leaf, "someone-followed")
	if err != nil {
		t.Fatalf("WritePlatformEvent: %v", err)
	}
	if v, _ := step.Outputs["result"].(string); v != "someone-followed" {
		t.Fatalf("expected result=someone-followed from on-platform-event's payload, got %#v", step.Outputs["result"])
	}
}

func TestHost_EntrypointMethodsFailOnEmptySlot(t *testing.T) {
	h := NewHost()
	if _, err := h.Tick(SlotOnAir, 1); err == nil {
		t.Fatal("expected ErrNotLoaded from Tick on an empty slot")
	}
	if _, err := h.Call(SlotOnAir, "x", nil); err == nil {
		t.Fatal("expected ErrNotLoaded from Call on an empty slot")
	}
	if _, err := h.WritePlatformEvent(SlotOnAir, "__inputs.platform.twitch.c.last_x", nil); err == nil {
		t.Fatal("expected ErrNotLoaded from WritePlatformEvent on an empty slot")
	}
	if _, err := h.Resolve(SlotOnAir, "x", nil); err == nil {
		t.Fatal("expected ErrNotLoaded from Resolve on an empty slot")
	}
	if _, err := h.Complete(SlotOnAir, []byte(`{}`)); err == nil {
		t.Fatal("expected ErrNotLoaded from Complete on an empty slot")
	}
}
