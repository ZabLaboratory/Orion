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
	"github.com/ZabLaboratory/Orion/internal/providers"
)

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
	switch kind {
	case "tick":
		opcode["id"] = "core.event.on-tick@1"
		opcode["outputs"] = []any{parityDataPort("delta_seconds", "core.json", false), parityExecPort("then")}
		entry["id"] = "tick"
		entry["node_id"] = "trigger-node"
	case "call":
		opcode["id"] = "core.operator.on-call@1"
	default:
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
	if err := h.Prepare(slot, id, "scene-"+id, "sha256:"+id, program, nil, nil, nil); err != nil {
		t.Fatalf("primitive=bluehost.prepare scenario=%s Engine B Host.Prepare: %v", id, err)
	}
	if _, err := h.Step(slot); err != nil {
		t.Fatalf("primitive=bluehost.step scenario=%s Engine B Host.Step: %v", id, err)
	}
	return h
}

func parityPrepareAPlatformIngress(t *testing.T, id, leaf string) (*Show, *Scene, *CanonicalEventIngress) {
	t.Helper()
	show := NewShow(NewComputeRegistry(), quietLogger())
	show.LoadExec(id, varsGraph(id), &compiler.RenderBundle{SceneVersion: "sha256:" + id}, parityEngineAEntryProgram("platform-event", leaf))
	if err := show.SetActive(id, nil); err != nil {
		show.Stop()
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=canonical-ingress Engine A SetActive: %v", err)
	}
	t.Cleanup(show.Stop)
	return show, show.Active(), NewCanonicalEventIngress(show)
}

func parityIngressErrorCode(err error) string {
	var ingressErr *EventIngressError
	if !errors.As(err, &ingressErr) {
		return ""
	}
	return ingressErr.Code
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

func parityNormalizeEventTrace(logs []string) []string {
	trace := make([]string, 0, len(logs))
	for _, log := range logs {
		value := strings.TrimPrefix(log, "print: ")
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		}
		trace = append(trace, value)
	}
	return trace
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
	aTrace := parityNormalizeEventTrace(aLogs)
	bTrace := parityNormalizeEventTrace(bLogs)
	if fmt.Sprint(aTrace) != "[one two]" || fmt.Sprint(bTrace) != "[one two]" {
		t.Fatalf("primitive=core.event.on-event@1 scenario=ordering FIFO trace is not [one two]: Engine A=%v Engine B=%v rawB=%v", aTrace, bTrace, bLogs)
	}
	if fmt.Sprint(aTrace) != fmt.Sprint(bTrace) {
		t.Fatalf("primitive=core.event.on-event@1 scenario=ordering normalized trace diverges: Engine A=%v Engine B=%v", aTrace, bTrace)
	}
}

func TestEngineABParity_EventDuplicateUsesCanonicalIngress(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_chat"
	event := parityEventEnvelope(t, "evt-duplicate", "platform.twitch", 1, "same")
	_, a, ingress := parityPrepareAPlatformIngress(t, "ab-event-duplicate-a", leaf)
	firstA, err := ingress.InjectPlatform(leaf, event)
	if err != nil || firstA.Status != "accepted" || firstA.RuntimeSequence != 1 || firstA.EventID != "evt-duplicate" {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=duplicate Engine A first receipt=%+v err=%v", firstA, err)
	}
	waitForState(t, a, "__vars.bp.result", `"same"`, 2*time.Second)
	duplicateA, err := ingress.InjectPlatform(leaf, event)
	if err != nil || duplicateA != firstA {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=duplicate Engine A receipt=%+v err=%v, want original receipt=%+v", duplicateA, err, firstA)
	}

	h := parityPrepareBHost(t, bluehost.SlotOnAir, "event-duplicate-b", parityBuildBEntrypointProgram(t, "platform-event", leaf))
	providers.ResetActiveIngress(h)
	t.Cleanup(func() { providers.ResetActiveIngress(h) })
	firstB, err := providers.InjectActivePlatform(h, leaf, event)
	if err != nil || firstB.Status != "accepted" || firstB.RuntimeSequence != 1 || firstB.EventID != "evt-duplicate" {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=duplicate Engine B first receipt=%+v err=%v", firstB, err)
	}
	step := parityHostStep(t, h, bluehost.SlotOnAir, "duplicate first")
	duplicateB, err := providers.InjectActivePlatform(h, leaf, event)
	if err != nil || duplicateB != firstB {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=duplicate Engine B receipt=%+v err=%v, want original receipt=%+v", duplicateB, err, firstB)
	}
	if got := step.Outputs["result"]; string(canonicalJSONForTest(got)) != `"same"` {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=duplicate Engine B result=%#v, want same", got)
	}
	if firstA != (EventIngressReceipt{Status: firstB.Status, RuntimeSequence: firstB.RuntimeSequence, EventID: firstB.EventID}) {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=duplicate receipt divergence: Engine A=%+v Engine B=%+v", firstA, firstB)
	}
}

func TestEngineABParity_EventGapAndOutOfOrderUseCanonicalIngress(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_chat"

	t.Run("gap-recovery-1-3-2", func(t *testing.T) {
		_, a, ingress := parityPrepareAPlatformIngress(t, "ab-event-gap-a", leaf)
		firstA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-gap-first", "platform.twitch", 1, "one"))
		if err != nil || firstA.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap Engine A seq=1 receipt=%+v err=%v", firstA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"one"`, 2*time.Second)
		_, gapAErr := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-gap-third", "platform.twitch", 3, "three"))
		if code := parityIngressErrorCode(gapAErr); code != "EVENT_SEQUENCE_GAP" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap Engine A code=%q err=%v, want EVENT_SEQUENCE_GAP", code, gapAErr)
		}
		secondA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-gap-second", "platform.twitch", 2, "two"))
		if err != nil || secondA.Status != "accepted" || secondA.RuntimeSequence != 2 {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap Engine A seq=2 receipt=%+v err=%v", secondA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"two"`, 2*time.Second)
		thirdA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-gap-third", "platform.twitch", 3, "three"))
		if err != nil || thirdA.Status != "accepted" || thirdA.RuntimeSequence != 3 {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap Engine A retry seq=3 receipt=%+v err=%v", thirdA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"three"`, 2*time.Second)

		h := parityPrepareBHost(t, bluehost.SlotOnAir, "event-gap-b", parityBuildBEntrypointProgram(t, "platform-event", leaf))
		providers.ResetActiveIngress(h)
		t.Cleanup(func() { providers.ResetActiveIngress(h) })
		firstB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-gap-first", "platform.twitch", 1, "one"))
		if err != nil || firstB.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap Engine B seq=1 receipt=%+v err=%v", firstB, err)
		}
		_ = parityHostStep(t, h, bluehost.SlotOnAir, "sequence-gap seq=1")
		_, gapBErr := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-gap-third", "platform.twitch", 3, "three"))
		if code := parityBlueErrorCode(gapBErr); code != "EVENT_SEQUENCE_GAP" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap Engine B code=%q err=%v, want EVENT_SEQUENCE_GAP", code, gapBErr)
		}
		secondB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-gap-second", "platform.twitch", 2, "two"))
		if err != nil || secondB.Status != "accepted" || secondB.RuntimeSequence != 2 {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap Engine B seq=2 receipt=%+v err=%v", secondB, err)
		}
		_ = parityHostStep(t, h, bluehost.SlotOnAir, "sequence-gap seq=2")
		thirdB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-gap-third", "platform.twitch", 3, "three"))
		if err != nil || thirdB.Status != "accepted" || thirdB.RuntimeSequence != 3 {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap Engine B retry seq=3 receipt=%+v err=%v", thirdB, err)
		}
		bStep := parityHostStep(t, h, bluehost.SlotOnAir, "sequence-gap retry seq=3")
		if firstA.RuntimeSequence != firstB.RuntimeSequence || secondA.RuntimeSequence != secondB.RuntimeSequence || thirdA.RuntimeSequence != thirdB.RuntimeSequence {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap receipt sequences diverge: A=[%+v %+v %+v] B=[%+v %+v %+v]", firstA, secondA, thirdA, firstB, secondB, thirdB)
		}
		if got := bStep.Outputs["result"]; string(canonicalJSONForTest(got)) != `"three"` {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=sequence-gap Engine B result=%#v, want three", got)
		}
	})

	t.Run("same-sequence-conflict", func(t *testing.T) {
		_, a, ingress := parityPrepareAPlatformIngress(t, "ab-event-conflict-a", leaf)
		firstA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-conflict-first", "platform.twitch", 1, "one"))
		if err != nil || firstA.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=conflict Engine A first receipt=%+v err=%v", firstA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"one"`, 2*time.Second)
		_, conflictAErr := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-conflict-second", "platform.twitch", 1, "again"))
		if code := parityIngressErrorCode(conflictAErr); code != "EVENT_SEQUENCE_CONFLICT" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=conflict Engine A code=%q err=%v, want EVENT_SEQUENCE_CONFLICT", code, conflictAErr)
		}

		h := parityPrepareBHost(t, bluehost.SlotOnAir, "event-conflict-b", parityBuildBEntrypointProgram(t, "platform-event", leaf))
		providers.ResetActiveIngress(h)
		t.Cleanup(func() { providers.ResetActiveIngress(h) })
		firstB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-conflict-first", "platform.twitch", 1, "one"))
		if err != nil || firstB.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=conflict Engine B first receipt=%+v err=%v", firstB, err)
		}
		_ = parityHostStep(t, h, bluehost.SlotOnAir, "conflict first")
		_, conflictBErr := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-conflict-second", "platform.twitch", 1, "again"))
		if code := parityBlueErrorCode(conflictBErr); code != "EVENT_SEQUENCE_CONFLICT" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=conflict Engine B code=%q err=%v, want EVENT_SEQUENCE_CONFLICT", code, conflictBErr)
		}
		if parityIngressErrorCode(conflictAErr) != parityBlueErrorCode(conflictBErr) || firstA.RuntimeSequence != firstB.RuntimeSequence {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=conflict admission diverges: A receipt=%+v code=%s B receipt=%+v code=%s", firstA, parityIngressErrorCode(conflictAErr), firstB, parityBlueErrorCode(conflictBErr))
		}
	})

	t.Run("out-of-order-before-first", func(t *testing.T) {
		_, a, ingress := parityPrepareAPlatformIngress(t, "ab-event-out-of-order-a", leaf)
		_, gapAErr := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-out-of-order", "platform.twitch", 2, "two"))
		if code := parityIngressErrorCode(gapAErr); code != "EVENT_SEQUENCE_GAP" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=out-of-order-before-first Engine A code=%q err=%v, want EVENT_SEQUENCE_GAP", code, gapAErr)
		}
		firstA, err := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-out-of-order-1", "platform.twitch", 1, "one"))
		if err != nil || firstA.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=out-of-order-before-first Engine A recovery receipt=%+v err=%v", firstA, err)
		}
		waitForState(t, a, "__vars.bp.result", `"one"`, 2*time.Second)

		h := parityPrepareBHost(t, bluehost.SlotOnAir, "event-out-of-order-b", parityBuildBEntrypointProgram(t, "platform-event", leaf))
		providers.ResetActiveIngress(h)
		t.Cleanup(func() { providers.ResetActiveIngress(h) })
		_, gapBErr := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-out-of-order", "platform.twitch", 2, "two"))
		if code := parityBlueErrorCode(gapBErr); code != "EVENT_SEQUENCE_GAP" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=out-of-order-before-first Engine B code=%q err=%v, want EVENT_SEQUENCE_GAP", code, gapBErr)
		}
		firstB, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-out-of-order-1", "platform.twitch", 1, "one"))
		if err != nil || firstB.Status != "accepted" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=out-of-order-before-first Engine B recovery receipt=%+v err=%v", firstB, err)
		}
		step := parityHostStep(t, h, bluehost.SlotOnAir, "out-of-order recovery")
		if parityIngressErrorCode(gapAErr) != parityBlueErrorCode(gapBErr) || firstA.RuntimeSequence != firstB.RuntimeSequence || string(canonicalJSONForTest(step.Outputs["result"])) != `"one"` {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=out-of-order-before-first admission/result diverges: A code=%s receipt=%+v B code=%s receipt=%+v result=%#v", parityIngressErrorCode(gapAErr), firstA, parityBlueErrorCode(gapBErr), firstB, step.Outputs["result"])
		}
	})
}

func TestEngineABParity_EventPerOriginSequenceAndCanonicalLeaf(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_chat"
	_, a, ingress := parityPrepareAPlatformIngress(t, "ab-event-origins-a", leaf)
	h := parityPrepareBHost(t, bluehost.SlotOnAir, "event-origins-b", parityBuildBEntrypointProgram(t, "platform-event", leaf))
	providers.ResetActiveIngress(h)
	t.Cleanup(func() { providers.ResetActiveIngress(h) })

	inputs := []struct {
		id      string
		origin  string
		seq     int
		payload string
	}{
		{id: "evt-origin-twitch-1", origin: "platform.twitch", seq: 1, payload: "twitch-one"},
		{id: "evt-origin-youtube-1", origin: "platform.youtube", seq: 1, payload: "youtube-one"},
		{id: "evt-origin-twitch-2", origin: "platform.twitch", seq: 2, payload: "twitch-two"},
	}
	for _, input := range inputs {
		event := parityEventEnvelope(t, input.id, input.origin, input.seq, input.payload)
		var envelope map[string]any
		if err := json.Unmarshal(event, &envelope); err != nil {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=per-origin envelope: %v", err)
		}
		if envelope["topic"] != "score" {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=per-origin topic=%#v, want score", envelope["topic"])
		}
		aReceipt, aErr := ingress.InjectPlatform(leaf, event)
		bReceipt, bErr := providers.InjectActivePlatform(h, leaf, event)
		if aErr != nil || bErr != nil || aReceipt.Status != "accepted" || bReceipt.Status != "accepted" || aReceipt.RuntimeSequence != bReceipt.RuntimeSequence {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=per-origin id=%s leaf=%s A=(%+v,%v) B=(%+v,%v)", input.id, leaf, aReceipt, aErr, bReceipt, bErr)
		}
		if aReceipt.EventID != input.id || bReceipt.EventID != input.id {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=per-origin id=%s receipt IDs A=%s B=%s", input.id, aReceipt.EventID, bReceipt.EventID)
		}
		waitForState(t, a, "__vars.bp.result", `"`+input.payload+`"`, 2*time.Second)
		_ = parityHostStep(t, h, bluehost.SlotOnAir, "per-origin "+input.id)
	}
	aValue, _ := a.state.Get("__vars.bp.result")
	if string(aValue) != `"twitch-two"` {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=per-origin Engine A result=%s, want twitch-two", aValue)
	}
	finalB := parityHostStep(t, h, bluehost.SlotOnAir, "per-origin final")
	if string(canonicalJSONForTest(finalB.Outputs["result"])) != `"twitch-two"` {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=per-origin Engine B result=%#v, want twitch-two", finalB.Outputs["result"])
	}
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

	h := bluehost.NewHost()
	t.Cleanup(func() {
		_ = h.Release(bluehost.SlotPreview, "test-cleanup")
		_ = h.Release(bluehost.SlotOnAir, "test-cleanup")
	})
	program := parityBuildBEntrypointProgram(t, "platform-event", leaf)
	for _, slot := range []struct {
		name string
		slot bluehost.Slot
	}{
		{name: "preview", slot: bluehost.SlotPreview},
		{name: "on-air", slot: bluehost.SlotOnAir},
	} {
		if err := h.Prepare(slot.slot, "platform-"+slot.name+"-b", "scene-platform-"+slot.name, "sha256:platform-"+slot.name, program, nil, nil, nil); err != nil {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine B Host.Prepare %s: %v", slot.name, err)
		}
		if _, err := h.Step(slot.slot); err != nil {
			t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine B Host.Step %s: %v", slot.name, err)
		}
	}
	providers.ResetActiveIngress(h)
	t.Cleanup(func() { providers.ResetActiveIngress(h) })
	receipt, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-active-platform", "platform.twitch", 1, map[string]any{"type": "chat", "payload": map[string]any{"text": "active"}}))
	if err != nil || receipt.Status != "accepted" {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine B providers.ActiveIngress receipt=%+v err=%v", receipt, err)
	}
	previewStep, err := h.Step(bluehost.SlotPreview)
	if err != nil {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine B Preview Step: %v", err)
	}
	if previewStep.Outputs["result"] != nil {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine B preview slot observed active ingress: %#v", previewStep.Outputs["result"])
	}
	bStep, err := h.Step(bluehost.SlotOnAir)
	if err != nil {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine B on-air Step: %v", err)
	}
	var decodedA any
	if err := json.Unmarshal(aValue, &decodedA); err != nil {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine A result is not JSON: %v", err)
	}
	if string(canonicalJSONForTest(decodedA)) != string(canonicalJSONForTest(bStep.Outputs["result"])) {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=active-only Engine A=%s Engine B=%#v", aValue, bStep.Outputs["result"])
	}
}

func TestEngineABParity_PlatformIngressUsesCanonicalActiveAdapter(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_chat"
	_, a, ingress := parityPrepareAPlatformIngress(t, "ab-platform-ingress-a", leaf)
	aReceipt, aErr := ingress.InjectPlatform(leaf, parityEventEnvelope(t, "evt-platform-ingress", "platform.twitch", 1, map[string]any{"type": "chat"}))
	if aErr != nil || aReceipt.Status != "accepted" || aReceipt.RuntimeSequence != 1 {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=platform-ingress-canonical Engine A receipt=%+v err=%v", aReceipt, aErr)
	}
	waitForState(t, a, "__vars.bp.result", `{"type":"chat"}`, 2*time.Second)

	h := parityPrepareBHost(t, bluehost.SlotOnAir, "platform-ingress-b", parityBuildBEntrypointProgram(t, "platform-event", leaf))
	providers.ResetActiveIngress(h)
	t.Cleanup(func() { providers.ResetActiveIngress(h) })
	bReceipt, err := providers.InjectActivePlatform(h, leaf, parityEventEnvelope(t, "evt-platform-ingress", "platform.twitch", 1, map[string]any{"type": "chat"}))
	if err != nil || bReceipt.Status != "accepted" || bReceipt.RuntimeSequence != 1 {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=platform-ingress-canonical Engine B ActiveIngress receipt=%+v err=%v", bReceipt, err)
	}
	bStep, err := h.Step(bluehost.SlotOnAir)
	if err != nil {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=platform-ingress-canonical Engine B Step: %v", err)
	}
	if aReceipt != (EventIngressReceipt{Status: bReceipt.Status, RuntimeSequence: bReceipt.RuntimeSequence, EventID: bReceipt.EventID}) {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=platform-ingress-canonical receipt divergence: Engine A=%+v Engine B=%+v", aReceipt, bReceipt)
	}
	aValue, _ := a.state.Get("__vars.bp.result")
	if got := canonicalJSONForTest(bStep.Outputs["result"]); string(got) != string(canonicalJSONForTest(json.RawMessage(aValue))) {
		t.Fatalf("primitive=core.event.on-platform-event@1 scenario=platform-ingress-canonical Engine A=%s Engine B=%s", aValue, got)
	}
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
	if err := preview.Prepare(bluehost.SlotPreview, "db-preview", "scene-db-preview", "sha256:db-preview", program, nil, nil,
		bluehost.NewEffectHandlers(bluehost.EffectDeps{DB: db, DataSources: ds}, blueruntime.Preview)); err != nil {
		t.Fatalf("primitive=core.db.query@1 scenario=preview-prepare Engine B bluehost.Host.Prepare: %v", err)
	}
	previewStep := parityBStep(t, preview, bluehost.SlotPreview, "db.query")
	if got := previewStep.Outputs["result"]; fmt.Sprint(got) != "1" {
		t.Fatalf("primitive=core.db.query@1 scenario=preview-query Engine B bluehost.Host result=%#v, want live count 1", got)
	}
	if got := queries.Load(); got != 1 {
		t.Fatalf("primitive=core.db.query@1 scenario=preview-query Engine B emitted %d DB queries, want one", got)
	}

	onAir := bluehost.NewHost()
	t.Cleanup(func() { _ = onAir.Release(bluehost.SlotOnAir, "test-cleanup") })
	if err := onAir.Prepare(bluehost.SlotOnAir, "db-on-air", "scene-db-on-air", "sha256:db-on-air", program, nil, nil,
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

	// Engine A's real PreviewSlot is exercised with the same read-only DB path
	// as Engine B: the preview must observe the same match count as on-air.
	previewSlot := NewPreviewSlot(context.Background(), NewComputeRegistry(), parityPreviewWire{}, quietLogger())
	previewSlot.SetEffects(&SceneEffects{Runner: newTestRunner(t), DB: db, DataSources: ds})
	previewSlot.Activate("ab-db-preview-a", effectsGraph("ab-db-preview-a"), &compiler.RenderBundle{SceneVersion: "sha256:effects-test"}, parityEngineADBProgram())
	t.Cleanup(previewSlot.Close)
	waitForState(t, previewSlot.Current(), "__vars.bp.result", `1`, 2*time.Second)
	aPreviewValue, _ := previewSlot.Current().state.Get("__vars.bp.result")
	if got := queries.Load(); got != 4 {
		t.Fatalf("primitive=core.db.query@1 scenario=preview-observable Engine A query count=%d, want B preview + B on-air + A on-air + A preview", got)
	}
	parityAssertInventoryResult(t, "core.db.query@1", "preview", aPreviewValue, previewStep.Outputs["result"])
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
			if err := h.Prepare(bluehost.SlotOnAir, "entry-"+tc.kind, "scene-entry-"+tc.kind, "sha256:entry-"+tc.kind, parityBuildBEntrypointProgram(t, tc.kind, leaf), nil, nil, nil); err != nil {
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
