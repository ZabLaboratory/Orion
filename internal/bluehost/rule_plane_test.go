package bluehost

import (
	"encoding/json"
	"sort"
	"sync"
	"testing"
)

const (
	markerRuleID = "45d43a69-34ff-42c9-bc5a-c333a93413e7"
	markerAppID  = "6d330786-1f44-4fac-9416-f935ee5e18b8"
)

type lockedOverlayMirror struct {
	mu    sync.Mutex
	calls []overlayMirrorCall
}

func (m *lockedOverlayMirror) EmitOverlayApp(appID string, running, onAir *bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, overlayMirrorCall{appID: appID, running: running, onAir: onAir})
}

func (m *lockedOverlayMirror) snapshot() []overlayMirrorCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]overlayMirrorCall(nil), m.calls...)
}

// markerOverlayProgram builds the published Marker rule's relevant portable
// shape: the two real cockpit entrypoint ids drive the real Marker manifest
// app id through core.overlay-app.set@1. There is no scene opcode or bundle.
func markerOverlayProgram(t *testing.T) []byte {
	t.Helper()
	execPort := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	dataPort := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": typ, "required": required}
	}
	program := map[string]any{
		"schema_version":   "blue.program.v1",
		"program_id":       "marker-stream-rule-proof",
		"compiler_version": "0.1.0",
		"runtime_abi":      "blue-runtime-abi.v1",
		"runtime_module":   map[string]any{"name": "blue-runtime-go", "version": "0.1.0"},
		"source_revision": map[string]any{
			"id": markerRuleID, "kind": "blueprint-version", "revision": 1,
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
			map[string]any{"id": "core.operator.on-call@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{dataPort("payload", "core.json", false), execPort("then")}},
			map[string]any{
				"id": "core.overlay-app.set@1", "kind": "control",
				"config": []any{
					dataPort("app_id", "core.string", false),
					dataPort("on_air", "core.json", false),
					dataPort("running", "core.json", false),
				},
				"inputs": []any{execPort("in")}, "outputs": []any{execPort("then")},
			},
		},
		"nodes": []any{
			map[string]any{"id": "marker-off", "opcode": "core.overlay-app.set@1", "config": map[string]any{"app_id": markerAppID, "running": false, "on_air": false}},
			map[string]any{"id": "marker-on", "opcode": "core.overlay-app.set@1", "config": map[string]any{"app_id": markerAppID, "running": true, "on_air": true}},
			map[string]any{"id": "off-call", "opcode": "core.operator.on-call@1", "config": map[string]any{}},
			map[string]any{"id": "on-call", "opcode": "core.operator.on-call@1", "config": map[string]any{}},
			map[string]any{"id": "start", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		"entrypoints": []any{
			map[string]any{"id": "marker_overlay_off", "kind": "call", "node_id": "off-call", "port": "then"},
			map[string]any{"id": "marker_overlay_on", "kind": "call", "node_id": "on-call", "port": "then"},
			map[string]any{"id": "start", "kind": "start", "node_id": "start", "port": "then"},
		},
		"exec_edges": []any{
			map[string]any{"from_node": "off-call", "from_port": "then", "to_node": "marker-off", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "on-call", "from_port": "then", "to_node": "marker-on", "to_port": "in", "sequence": 0},
		},
		"data_edges": []any{}, "data_literals": []any{}, "effects": []any{},
		"requires": []any{}, "topics": []any{}, "timers": []any{},
		"state": map[string]any{"variables": []any{}, "outputs": []any{}},
	}
	for _, key := range []string{"opcodes", "nodes", "entrypoints"} {
		sortByID(program[key].([]any))
	}
	sort.Slice(program["exec_edges"].([]any), func(i, j int) bool {
		a := program["exec_edges"].([]any)[i].(map[string]any)["from_node"].(string)
		b := program["exec_edges"].([]any)[j].(map[string]any)["from_node"].(string)
		return a < b
	})
	program["program_digest"] = digestOf(program)
	out, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal Marker program: %v", err)
	}
	return out
}

// TestRulePlane_MarkerOnOffAcrossSceneSlots is the targeted Marker proof at
// the Orion runtime boundary. The rule is promoted once, no Preview/OnAir
// scene is loaded, and the two cockpit calls drive the exact Marker app id.
// The plane is therefore demonstrably global rather than copied into a slot.
func TestRulePlane_MarkerOnOffAcrossSceneSlots(t *testing.T) {
	mirror := &lockedOverlayMirror{}
	plane := NewRulePlane(nil, nil, EffectDeps{OverlayMirror: mirror}, 1, nil)
	defer plane.Stop()

	program := markerOverlayProgram(t)
	var identity struct {
		ProgramDigest string `json:"program_digest"`
	}
	if err := json.Unmarshal(program, &identity); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if err := plane.Promote(markerRuleID, identity.ProgramDigest, program); err != nil {
		t.Fatalf("Promote Marker: %v", err)
	}

	contracts := plane.Contracts()
	if len(contracts) != 1 || contracts[0].RuleID != markerRuleID || len(contracts[0].Triggers) != 2 {
		t.Fatalf("Marker contracts = %#v, want one global rule with two triggers", contracts)
	}
	if !plane.HasTrigger(markerRuleID, "marker_overlay_on") || !plane.HasTrigger(markerRuleID, "marker_overlay_off") {
		t.Fatalf("Marker on/off trigger missing: %#v", contracts[0].Triggers)
	}

	if _, err := plane.Call(markerRuleID, "marker_overlay_on", nil); err != nil {
		t.Fatalf("marker_overlay_on: %v", err)
	}
	calls := mirror.snapshot()
	if len(calls) != 1 || calls[0].appID != markerAppID || calls[0].running == nil || !*calls[0].running || calls[0].onAir == nil || !*calls[0].onAir {
		t.Fatalf("Marker ON mirror calls = %#v", calls)
	}

	if _, err := plane.Call(markerRuleID, "marker_overlay_off", nil); err != nil {
		t.Fatalf("marker_overlay_off: %v", err)
	}
	calls = mirror.snapshot()
	if len(calls) != 2 || calls[1].appID != markerAppID || calls[1].running == nil || *calls[1].running || calls[1].onAir == nil || *calls[1].onAir {
		t.Fatalf("Marker OFF mirror calls = %#v", calls)
	}
}
