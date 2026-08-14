package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// parityNonEquivalenceError is deliberately a test-only report type. It is
// used whenever Engine A and Engine B expose different contracts; keeping
// the primitive and scenario in the value prevents a direct-runtime proof
// from being mistaken for host parity.
type parityNonEquivalenceError struct {
	Primitive string
	Scenario  string
	EngineA   string
	EngineB   string
}

func (e parityNonEquivalenceError) Error() string {
	return fmt.Sprintf("primitive=%s scenario=%s Engine A=%s Engine B=%s", e.Primitive, e.Scenario, e.EngineA, e.EngineB)
}

func parityExecPort(name string) map[string]any {
	return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
}

func parityDataPort(name, typ string, required bool) map[string]any {
	return map[string]any{"name": name, "kind": "data", "type": typ, "required": required}
}

func paritySortByID(items []any) {
	sort.Slice(items, func(i, j int) bool {
		a, _ := items[i].(map[string]any)["id"].(string)
		b, _ := items[j].(map[string]any)["id"].(string)
		return a < b
	})
}

func parityBuildProgram(t *testing.T, id string, opcodes, nodes, entrypoints, execEdges, dataEdges, dataLiterals, topics []any, variables []any) []byte {
	t.Helper()
	program := map[string]any{
		"schema_version":   blueruntime.ProgramSchema,
		"program_id":       id,
		"compiler_version": blueruntime.CompilerVersion,
		"runtime_abi":      blueruntime.RuntimeABI,
		"runtime_module":   map[string]any{"name": blueruntime.RuntimeModule, "version": blueruntime.RuntimeModuleVer},
		"source_revision": map[string]any{
			"id": id, "kind": "blueprint-version", "revision": 1,
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
		"opcodes":       opcodes,
		"nodes":         nodes,
		"entrypoints":   entrypoints,
		"exec_edges":    execEdges,
		"data_edges":    dataEdges,
		"data_literals": dataLiterals,
		"effects":       []any{},
		"requires":      []any{},
		"topics":        topics,
		"timers":        []any{},
		"state": map[string]any{
			"variables": variables,
			"outputs":   []any{map[string]any{"name": "result", "type": "core.json"}},
		},
	}
	paritySortByID(opcodes)
	paritySortByID(nodes)
	paritySortByID(entrypoints)
	program["program_digest"] = digestOfForTest(program)
	out, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal Blue fixture %s: %v", id, err)
	}
	return out
}

func parityBuildBEntrypointProgram(t *testing.T, kind string, leaf string) []byte {
	t.Helper()
	opcode := map[string]any{"id": "core.event.on-" + kind + "@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityDataPort("payload", "core.json", false), parityExecPort("then")}}
	entry := map[string]any{"id": "trigger", "kind": kind, "node_id": "trigger-node", "port": "then"}
	if kind == "tick" {
		opcode["id"] = "core.event.on-tick@1"
		opcode["outputs"] = []any{parityDataPort("delta_seconds", "core.json", false), parityExecPort("then")}
		entry["id"] = "tick"
		entry["node_id"] = "trigger-node"
	} else if kind == "call" {
		opcode["id"] = "core.operator.on-call@1"
	} else {
		opcode["id"] = "core.event.on-platform-event@1"
		entry["leaf"] = leaf
	}
	dataPort := "payload"
	if kind == "tick" {
		dataPort = "delta_seconds"
	}
	return parityBuildProgram(t, "ab-parity-entry-"+kind,
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			opcode,
			map[string]any{
				"id": "core.variable.set@1", "kind": "pure",
				"config":  []any{parityDataPort("variable", "core.string", true)},
				"inputs":  []any{parityDataPort("value", "core.json", true), parityExecPort("in")},
				"outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")},
			},
		},
		[]any{
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "trigger-node", "opcode": opcode["id"], "config": map[string]any{}},
		},
		[]any{
			map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"},
			entry,
		},
		[]any{map[string]any{"from_node": "trigger-node", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0}},
		[]any{map[string]any{"from_node": "trigger-node", "from_port": dataPort, "to_node": "mark", "to_port": "value"}},
		[]any{}, []any{}, []any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
	)
}

func parityBuildBDBProgram(t *testing.T) []byte {
	t.Helper()
	return parityBuildProgram(t, "ab-parity-db",
		[]any{
			map[string]any{"id": "core.db.query@1", "kind": "control", "config": []any{parityDataPort("datasource", "core.string", true)}, "inputs": []any{parityDataPort("descriptor", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("count", "core.json", false), parityDataPort("elapsed_ms", "core.json", false), parityDataPort("rows", "core.json", false), parityExecPort("error"), parityExecPort("then")}},
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "db", "opcode": "core.db.query@1", "config": map[string]any{"datasource": "truth"}},
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		[]any{map[string]any{"from_node": "db", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0}, map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "db", "to_port": "in", "sequence": 0}},
		[]any{map[string]any{"from_node": "db", "from_port": "count", "to_node": "mark", "to_port": "value"}},
		[]any{map[string]any{"node_id": "db", "port": "descriptor", "value": map[string]any{"table": "players", "select": []any{"id"}}}},
		[]any{},
		[]any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
	)
}

func parityEngineAEntryProgram(kind, leaf string) *ExecProgram {
	dataPort := "payload"
	entry := ExecEntry{Kind: EntryOnPlatformEvent, Event: leaf, Node: "trigger", Target: ExecTarget{Node: "mark"}}
	switch kind {
	case "tick":
		dataPort = "delta_seconds"
		entry = ExecEntry{Kind: EntryOnTick, Node: "trigger", Target: ExecTarget{Node: "mark"}}
	case "call":
		entry = ExecEntry{Kind: EntryOnCall, Node: "trigger", Target: ExecTarget{Node: "mark"}}
	}
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"mark": setFromPin("mark", "result", "trigger", dataPort, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"trigger": entry,
		},
	}
}

func parityEngineADBProgram() *ExecProgram {
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"db": {ID: "db", Op: OpDBQuery, Config: map[string]json.RawMessage{
				"datasource": raw(`"truth"`),
				"descriptor": raw(`{"table":"players","select":["id"]}`),
			}, Next: map[string]ExecTarget{"then": {Node: "mark"}}},
			"mark": setFromPin("mark", "result", "db", "count", nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "db"}},
		},
	}
}

func parityBuildBTopicPrintProgram(t *testing.T) []byte {
	t.Helper()
	return parityBuildProgram(t, "ab-parity-topic",
		[]any{
			map[string]any{"id": "core.event.on-event@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityDataPort("payload", "core.json", false), parityExecPort("then")}},
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.print@1", "kind": "pure", "config": []any{}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "event-node", "opcode": "core.event.on-event@1", "config": map[string]any{}},
			map[string]any{"id": "print", "opcode": "core.print@1", "config": map[string]any{}},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{
			map[string]any{"id": "score", "kind": "topic", "topic": "score", "node_id": "event-node", "port": "then"},
			map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"},
		},
		[]any{map[string]any{"from_node": "event-node", "from_port": "then", "to_node": "print", "to_port": "in", "sequence": 0}},
		[]any{map[string]any{"from_node": "event-node", "from_port": "payload", "to_node": "print", "to_port": "value"}},
		[]any{},
		[]any{map[string]any{"name": "score", "payload_type": "core.json"}},
		[]any{},
	)
}

func parityEngineAEventPrintProgram() *ExecProgram {
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"print": {ID: "print", Op: OpPrint, Data: []ExecDataInput{{Port: "value", From: "event", FromPort: "payload"}}},
		},
		Entrypoints: map[string]ExecEntry{
			"score": {Kind: EntryOnEvent, Event: "score", Node: "event", Target: ExecTarget{Node: "print"}},
		},
	}
}

func parityDigestValue(value any) string {
	sum := sha256.Sum256(canonicalJSONForTest(value))
	return fmt.Sprintf("sha256:%x", sum)
}

func parityEventEnvelope(t *testing.T, eventID, origin string, sequence int, payload any) []byte {
	t.Helper()
	event := map[string]any{
		"schema_version":  blueruntime.EventSchema,
		"event_id":        eventID,
		"origin":          origin,
		"source_sequence": json.Number(fmt.Sprintf("%d", sequence)),
		"topic":           "score",
		"occurred_at_ms":  json.Number(fmt.Sprintf("%d", sequence)),
		"payload":         payload,
		"payload_digest":  parityDigestValue(payload),
		"correlation_id":  "parity-correlation",
	}
	event["event_digest"] = parityDigestValue(event)
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event envelope %s: %v", eventID, err)
	}
	return data
}

func parityPrepareBHost(t *testing.T, slot bluehost.Slot, id string, program []byte) *bluehost.Host {
	t.Helper()
	h := bluehost.NewHost()
	t.Cleanup(func() { _ = h.Release(slot, "test-cleanup") })
	if err := h.Prepare(slot, id, "sha256:"+id, program, nil, nil, nil); err != nil {
		t.Fatalf("primitive=bluehost.prepare scenario=%s Engine B Host.Prepare: %v", id, err)
	}
	if _, err := h.Step(slot); err != nil {
		t.Fatalf("primitive=bluehost.step scenario=%s Engine B Host.Step: %v", id, err)
	}
	return h
}

func parityLogs(step blueruntime.StepResult) []string {
	values, _ := step.Variables["__logs__"].([]any)
	logs := make([]string, 0, len(values))
	for _, value := range values {
		if log, ok := value.(string); ok {
			logs = append(logs, log)
		}
	}
	return logs
}

func parityWaitForALogs(t *testing.T, sc *Scene, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		value, ok := sc.state.Get("__debug.bp.print")
		if ok {
			var logs []string
			if json.Unmarshal(value, &logs) == nil && len(logs) >= want {
				return logs
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	value, _ := sc.state.Get("__debug.bp.print")
	t.Fatalf("primitive=core.event.on-event@1 scenario=ordering Engine A logs=%s, want at least %d entries", value, want)
	return nil
}

func parityAssertTypedGap(t *testing.T, gap parityNonEquivalenceError) {
	t.Helper()
	if gap.Primitive == "" || gap.Scenario == "" || gap.EngineA == "" || gap.EngineB == "" {
		t.Fatalf("typed parity gap is incomplete: %#v", gap)
	}
	t.Logf("expected typed non-equivalence: %v", gap)
}

func parityBlueErrorCode(err error) string {
	var blueErr *blueruntime.Error
	if !errors.As(err, &blueErr) {
		return ""
	}
	return blueErr.Code
}

func TestEngineABParity_EventFIFOOrderingThroughHost(t *testing.T) {
	a := execScene(t, "ab-event-order-a", parityEngineAEventPrintProgram())
	startScene(t, a)
	if !a.Input(InputMsg{Path: eventsPrefix + "score", Value: raw(`"one"`), Source: "event:test"}) ||
		!a.Input(InputMsg{Path: eventsPrefix + "score", Value: raw(`"two"`), Source: "event:test"}) {
		t.Fatal("primitive=core.event.on-event@1 scenario=ordering Engine A rejected an input")
	}
	aLogs := parityWaitForALogs(t, a, 2, 2*time.Second)
	if fmt.Sprint(aLogs) != "[one two]" {
		t.Fatalf("primitive=core.event.on-event@1 scenario=ordering Engine A logs=%v, want [one two]", aLogs)
	}

	h := parityPrepareBHost(t, bluehost.SlotOnAir, "event-order-b", parityBuildBTopicPrintProgram(t))
	for sequence, value := range []string{"one", "two"} {
		receipt, err := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-order-"+value, "platform.twitch", sequence+1, value))
		if err != nil || receipt.Status != "accepted" || receipt.RuntimeSequence != uint64(sequence+1) {
			t.Fatalf("primitive=core.event.on-event@1 scenario=ordering Engine B Host.Dispatch receipt=%+v err=%v", receipt, err)
		}
		if _, err := h.Step(bluehost.SlotOnAir); err != nil {
			t.Fatalf("primitive=core.event.on-event@1 scenario=ordering Engine B Host.Step: %v", err)
		}
	}
	bStep, err := h.Step(bluehost.SlotOnAir)
	if err != nil {
		t.Fatalf("primitive=core.event.on-event@1 scenario=ordering Engine B Host.Step final: %v", err)
	}
	bLogs := parityLogs(bStep)
	if len(bLogs) != 2 || bLogs[0] != "print: 'one'" || bLogs[1] != "print: 'two'" {
		t.Fatalf("primitive=core.event.on-event@1 scenario=ordering Engine B Host logs=%v, want ordered one/two", bLogs)
	}
	if strings.TrimPrefix(bLogs[0], "print: '") != "one'" {
		t.Fatalf("primitive=core.event.on-event@1 scenario=ordering Engine B log normalization changed: %v", bLogs)
	}
}

func TestEngineABParity_EventDuplicateIsTypedNonEquivalent(t *testing.T) {
	a := execScene(t, "ab-event-duplicate-a", parityEngineAEventPrintProgram())
	startScene(t, a)
	message := InputMsg{Path: eventsPrefix + "score", Value: raw(`"same"`), Source: "event:test"}
	if !a.Input(message) || !a.Input(message) {
		t.Fatal("primitive=core.event.on-event@1 scenario=duplicate Engine A rejected a repeated InputMsg")
	}
	aLogs := parityWaitForALogs(t, a, 2, 2*time.Second)
	if len(aLogs) != 2 {
		t.Fatalf("primitive=core.event.on-event@1 scenario=duplicate Engine A logs=%v, want two fires", aLogs)
	}

	h := parityPrepareBHost(t, bluehost.SlotOnAir, "event-duplicate-b", parityBuildBTopicPrintProgram(t))
	event := parityEventEnvelope(t, "evt-duplicate", "platform.twitch", 1, "same")
	first, err := h.Dispatch(bluehost.SlotOnAir, event)
	if err != nil || first.Status != "accepted" {
		t.Fatalf("primitive=core.event.on-event@1 scenario=duplicate Engine B first Host.Dispatch receipt=%+v err=%v", first, err)
	}
	step, err := h.Step(bluehost.SlotOnAir)
	if err != nil {
		t.Fatalf("primitive=core.event.on-event@1 scenario=duplicate Engine B Host.Step: %v", err)
	}
	duplicate, err := h.Dispatch(bluehost.SlotOnAir, event)
	if err != nil || duplicate.Status != "duplicate" {
		t.Fatalf("primitive=core.event.on-event@1 scenario=duplicate Engine B duplicate Host.Dispatch receipt=%+v err=%v", duplicate, err)
	}
	bLogs := parityLogs(step)
	if len(bLogs) != 1 {
		t.Fatalf("primitive=core.event.on-event@1 scenario=duplicate Engine B logs=%v, want one fire after duplicate admission", bLogs)
	}
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.event.on-event@1",
		Scenario:  "duplicate",
		EngineA:   "InputMsg has no event_id/digest dedup seam and fires twice",
		EngineB:   "bluehost.Host.Dispatch returns duplicate and queues one event",
	})
}

func TestEngineABParity_EventGapAndOutOfOrderAreTyped(t *testing.T) {
	a := execScene(t, "ab-event-gap-a", parityEngineAEventPrintProgram())
	startScene(t, a)
	if !a.Input(InputMsg{Path: eventsPrefix + "score", Value: raw(`{"source_sequence":3,"value":"gap"}`), Source: "event:test"}) {
		t.Fatal("primitive=core.event.on-event@1 scenario=sequence-gap Engine A rejected payload carrying an unvalidated sequence field")
	}
	if len(parityWaitForALogs(t, a, 1, 2*time.Second)) != 1 {
		t.Fatal("primitive=core.event.on-event@1 scenario=sequence-gap Engine A did not observe the payload")
	}

	h := parityPrepareBHost(t, bluehost.SlotOnAir, "event-gap-b", parityBuildBTopicPrintProgram(t))
	first, err := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-gap-first", "platform.twitch", 1, "one"))
	if err != nil || first.Status != "accepted" {
		t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap Engine B first Host.Dispatch receipt=%+v err=%v", first, err)
	}
	if _, err := h.Step(bluehost.SlotOnAir); err != nil {
		t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap Engine B first Host.Step: %v", err)
	}
	_, gapErr := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-gap-third", "platform.twitch", 3, "three"))
	if code := parityBlueErrorCode(gapErr); code != "EVENT_SEQUENCE_GAP" {
		t.Fatalf("primitive=core.event.on-event@1 scenario=sequence-gap Engine B code=%q err=%v, want EVENT_SEQUENCE_GAP", code, gapErr)
	}
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.event.on-event@1",
		Scenario:  "sequence-gap",
		EngineA:   "InputMsg carries no source_sequence/event_digest contract and accepts the payload",
		EngineB:   "bluehost.Host.Dispatch rejects source_sequence=3 with EVENT_SEQUENCE_GAP",
	})

	hConflict := parityPrepareBHost(t, bluehost.SlotOnAir, "event-conflict-b", parityBuildBTopicPrintProgram(t))
	if _, err := hConflict.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-conflict-first", "platform.twitch", 1, "one")); err != nil {
		t.Fatalf("primitive=core.event.on-event@1 scenario=out-of-order Engine B first Host.Dispatch: %v", err)
	}
	_, conflictErr := hConflict.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-conflict-second", "platform.twitch", 1, "again"))
	if code := parityBlueErrorCode(conflictErr); code != "EVENT_SEQUENCE_CONFLICT" {
		t.Fatalf("primitive=core.event.on-event@1 scenario=out-of-order Engine B code=%q err=%v, want EVENT_SEQUENCE_CONFLICT", code, conflictErr)
	}
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.event.on-event@1",
		Scenario:  "out-of-order",
		EngineA:   "InputMsg has no source sequence admission seam",
		EngineB:   "bluehost.Host.Dispatch rejects a repeated source_sequence with EVENT_SEQUENCE_CONFLICT",
	})
}

func TestEngineABParity_ActiveOnlyPlatformEventUsesHostSlots(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_chat"
	payload := raw(`{"type":"chat","payload":{"text":"active"}}`)
	a := execScene(t, "ab-platform-active-a", parityEngineAEntryProgram("platform-event", leaf))
	a.GateTriggers()
	startScene(t, a)
	if !a.Input(InputMsg{Path: leaf, Value: payload, Source: "platform:twitch"}) {
		t.Fatal("primitive=core.event.on-platform-event@1 scenario=active-only Engine A rejected off-air platform input")
	}
	time.Sleep(25 * time.Millisecond)
	if _, ok := a.state.Get("__vars.bp.result"); ok {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine A fired while gated off-air")
	}
	if !a.SetOnAir(true) || !a.Input(InputMsg{Path: leaf, Value: payload, Source: "platform:twitch"}) {
		t.Fatal("primitive=core.event.on-platform-event@1 scenario=active-only Engine A could not activate and deliver event")
	}
	waitForState(t, a, "__vars.bp.result", string(payload), 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.result")

	h := parityPrepareBHost(t, bluehost.SlotOnAir, "platform-active-b", parityBuildBEntrypointProgram(t, "platform-event", leaf))
	if _, err := h.WritePlatformEvent(bluehost.SlotPreview, leaf, map[string]any{"inactive": true}); !errors.Is(err, bluehost.ErrNotLoaded) {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine B preview route err=%v, want bluehost.ErrNotLoaded", err)
	}
	bStep, err := h.WritePlatformEvent(bluehost.SlotOnAir, leaf, map[string]any{"type": "chat", "payload": map[string]any{"text": "active"}})
	if err != nil {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine B bluehost.Host.WritePlatformEvent: %v", err)
	}
	var decodedA any
	if err := json.Unmarshal(aValue, &decodedA); err != nil {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine A result is not JSON: %v", err)
	}
	if string(canonicalJSONForTest(decodedA)) != string(canonicalJSONForTest(bStep.Outputs["result"])) {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine A=%s Engine B=%#v", aValue, bStep.Outputs["result"])
	}
}

func TestEngineABParity_PlatformIngressDispatchGapIsTyped(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_chat"
	a := execScene(t, "ab-platform-ingress-a", parityEngineAEntryProgram("platform-event", leaf))
	startScene(t, a)
	if !a.Input(InputMsg{Path: leaf, Value: raw(`{"type":"chat"}`), Source: "platform:twitch"}) {
		t.Fatal("primitive=core.event.on-platform-event@1 scenario=platform-ingress-host-dispatch Engine A rejected canonical platform leaf")
	}
	waitForState(t, a, "__vars.bp.result", `{"type":"chat"}`, 2*time.Second)

	h := parityPrepareBHost(t, bluehost.SlotOnAir, "platform-ingress-b", parityBuildBEntrypointProgram(t, "platform-event", leaf))
	_, err := h.Dispatch(bluehost.SlotOnAir, parityEventEnvelope(t, "evt-platform-ingress", "platform.twitch", 1, map[string]any{"type": "chat"}))
	if code := parityBlueErrorCode(err); code != "EVENT_TOPIC_UNKNOWN" {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=platform-ingress-host-dispatch Engine B Host.Dispatch code=%q err=%v, want EVENT_TOPIC_UNKNOWN", code, err)
	}
	parityAssertTypedGap(t, parityNonEquivalenceError{
		Primitive: "core.event.on-platform-event@1",
		Scenario:  "platform-ingress-host-dispatch",
		EngineA:   "InputMsg writes the canonical platform leaf and fires the entrypoint",
		EngineB:   "real bluehost.Host.Dispatch is topic-envelope ingress and rejects the platform-only program",
	})
}

type parityPreviewWire struct{}

func (parityPreviewWire) MirrorFor(string, string, *compiler.RenderBundle) SceneMirror {
	return parityPreviewMirror{}
}

func (parityPreviewWire) SetActive(string) {}
func (parityPreviewWire) Drop(string)      {}

type parityPreviewMirror struct{}

func (parityPreviewMirror) Forward(SubscriberMsg) {}

func parityBStep(t *testing.T, h *bluehost.Host, slot bluehost.Slot, label string) blueruntime.StepResult {
	t.Helper()
	if _, err := h.Step(slot); err != nil {
		t.Fatalf("primitive=host.%s scenario=on-start Engine B Host.Step: %v", label, err)
	}
	step, err := h.Step(slot)
	if err != nil {
		t.Fatalf("primitive=host.%s scenario=trigger Engine B Host.Step: %v", label, err)
	}
	return step
}

func TestEngineABParity_DBQueryObservableAndPreviewNoQuery(t *testing.T) {
	var queries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		queries.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rows":[{"id":"p1"}],"count":1,"elapsed_ms":3}`))
	}))
	defer srv.Close()

	db := effects.NewDBQueryClient(srv.URL, "parity-token", nil)
	ds := map[string]effects.DataSource{"truth": {Name: "truth", Svc: "truth"}}
	program := parityBuildBDBProgram(t)

	preview := bluehost.NewHost()
	t.Cleanup(func() { _ = preview.Release(bluehost.SlotPreview, "test-cleanup") })
	if err := preview.Prepare(bluehost.SlotPreview, "db-preview", "sha256:db-preview", program, nil, nil,
		bluehost.NewEffectHandlers(bluehost.EffectDeps{DB: db, DataSources: ds}, blueruntime.Preview)); err != nil {
		t.Fatalf("primitive=core.db.query@1 scenario=preview-prepare Engine B bluehost.Host.Prepare: %v", err)
	}
	previewStep := parityBStep(t, preview, bluehost.SlotPreview, "db.query")
	if got := previewStep.Outputs["result"]; fmt.Sprint(got) != "0" {
		t.Fatalf("primitive=core.db.query@1 scenario=preview-no-query Engine B bluehost.Host result=%#v, want zero synthetic count", got)
	}
	if got := queries.Load(); got != 0 {
		t.Fatalf("primitive=core.db.query@1 scenario=preview-no-query Engine B emitted %d DB queries, want zero", got)
	}

	onAir := bluehost.NewHost()
	t.Cleanup(func() { _ = onAir.Release(bluehost.SlotOnAir, "test-cleanup") })
	if err := onAir.Prepare(bluehost.SlotOnAir, "db-on-air", "sha256:db-on-air", program, nil, nil,
		bluehost.NewEffectHandlers(bluehost.EffectDeps{DB: db, DataSources: ds}, blueruntime.Execute)); err != nil {
		t.Fatalf("primitive=core.db.query@1 scenario=on-air-prepare Engine B bluehost.Host.Prepare: %v", err)
	}
	bOnAir := parityBStep(t, onAir, bluehost.SlotOnAir, "db.query")
	bCount, ok := bOnAir.Outputs["result"].(json.Number)
	if !ok || bCount.String() != "1" {
		t.Fatalf("primitive=core.db.query@1 scenario=on-air-observable Engine B result=%#v, want count 1", bOnAir.Outputs["result"])
	}

	aOnAir := effectsScene(t, "ab-db-on-air", parityEngineADBProgram(), &SceneEffects{
		Runner:      newTestRunner(t),
		DB:          db,
		DataSources: ds,
	})
	startScene(t, aOnAir)
	mustFire(t, aOnAir, "start")
	waitForState(t, aOnAir, "__vars.bp.result", `1`, 2*time.Second)
	aCount, _ := aOnAir.state.Get("__vars.bp.result")
	if string(aCount) != bCount.String() {
		t.Fatalf("primitive=core.db.query@1 scenario=on-air-observable Engine A result=%s Engine B result=%s", aCount, bCount)
	}

	// Engine A's real PreviewSlot is intentionally exercised as well. The
	// current A seam performs the query; this is a typed, expected gap against
	// Blue's zero-query preview policy, not an omitted comparison.
	previewSlot := NewPreviewSlot(context.Background(), NewComputeRegistry(), parityPreviewWire{}, quietLogger())
	previewSlot.SetEffects(&SceneEffects{Runner: newTestRunner(t), DB: db, DataSources: ds})
	previewSlot.Activate("ab-db-preview-a", effectsGraph("ab-db-preview-a"), &compiler.RenderBundle{SceneVersion: "sha256:effects-test"}, parityEngineADBProgram())
	t.Cleanup(previewSlot.Close)
	waitForState(t, previewSlot.Current(), "__vars.bp.result", `1`, 2*time.Second)
	if got := queries.Load(); got != 3 {
		t.Fatalf("primitive=core.db.query@1 scenario=preview-observable Engine A query count=%d, want one B on-air + one A on-air + one A preview", got)
	}
	divergence := parityNonEquivalenceError{
		Primitive: "core.db.query@1",
		Scenario:  "preview-no-world-effect",
		EngineA:   "PreviewSlot performs the observable DB query",
		EngineB:   "bluehost.Host SlotPreview returns synthetic count=0 without a query",
	}
	var typed *parityNonEquivalenceError
	copyOf := divergence
	typed = &copyOf
	if typed.Primitive == "" || typed.Scenario == "" {
		t.Fatalf("typed preview divergence lost its primitive/scenario: %v", typed)
	}
	t.Logf("expected typed non-equivalence: %v", typed)
}

func TestEngineABParity_HostEntrypointMatrix(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_chat"
	payload := `{"type":"chat","payload":{"text":"hello"}}`
	cases := []struct {
		name       string
		kind       string
		input      func(*Scene)
		hostInvoke func(*bluehost.Host) (blueruntime.StepResult, error)
		want       string
	}{
		{name: "on-tick", kind: "tick", input: func(sc *Scene) {
			sc.Input(InputMsg{Path: tickPath, Value: raw(`1000`), Source: "system:tick", IsSystem: true})
			sc.Input(InputMsg{Path: tickPath, Value: raw(`2500`), Source: "system:tick", IsSystem: true})
		}, hostInvoke: func(h *bluehost.Host) (blueruntime.StepResult, error) { return h.Tick(bluehost.SlotOnAir, 1.5) }, want: "1.5"},
		{name: "on-platform-event", kind: "platform-event", input: func(sc *Scene) {
			sc.Input(InputMsg{Path: leaf, Value: raw(payload), Source: "platform:twitch"})
		}, hostInvoke: func(h *bluehost.Host) (blueruntime.StepResult, error) {
			return h.WritePlatformEvent(bluehost.SlotOnAir, leaf, map[string]any{"type": "chat", "payload": map[string]any{"text": "hello"}})
		}, want: payload},
		{name: "on-call", kind: "call", input: func(sc *Scene) {
			sc.FireOnCall("bp/trigger", raw(payload))
		}, hostInvoke: func(h *bluehost.Host) (blueruntime.StepResult, error) {
			return h.Call(bluehost.SlotOnAir, "trigger", map[string]any{"type": "chat", "payload": map[string]any{"text": "hello"}})
		}, want: payload},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := execScene(t, "ab-"+tc.name, parityEngineAEntryProgram(tc.kind, leaf))
			startScene(t, a)
			tc.input(a)
			waitForState(t, a, "__vars.bp.result", tc.want, 2*time.Second)
			aValue, _ := a.state.Get("__vars.bp.result")

			h := bluehost.NewHost()
			t.Cleanup(func() { _ = h.Release(bluehost.SlotOnAir, "test-cleanup") })
			if err := h.Prepare(bluehost.SlotOnAir, "entry-"+tc.kind, "sha256:entry-"+tc.kind, parityBuildBEntrypointProgram(t, tc.kind, leaf), nil, nil, nil); err != nil {
				t.Fatalf("primitive=core.event.%s scenario=matrix-prepare Engine B bluehost.Host: %v", tc.kind, err)
			}
			if _, err := h.Step(bluehost.SlotOnAir); err != nil {
				t.Fatalf("primitive=core.event.%s scenario=matrix-on-start Engine B bluehost.Host.Step: %v", tc.kind, err)
			}
			bStep, err := tc.hostInvoke(h)
			if err != nil {
				t.Fatalf("primitive=core.event.%s scenario=matrix-trigger Engine B bluehost.Host: %v", tc.kind, err)
			}
			bValue := bStep.Outputs["result"]
			var decodedA any
			decoder := json.NewDecoder(bytes.NewReader(aValue))
			decoder.UseNumber()
			if err := decoder.Decode(&decodedA); err != nil {
				t.Fatalf("primitive=core.event.%s scenario=matrix-observable Engine A result is not JSON: %v", tc.kind, err)
			}
			if string(canonicalJSONForTest(decodedA)) != string(canonicalJSONForTest(bValue)) {
				t.Fatalf("primitive=core.event.%s scenario=matrix-observable Engine A=%s Engine B=%#v", tc.kind, aValue, bValue)
			}
		})
	}
}
