package runtime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// ENGINE-B-PARITY-ORION differential harness: the SAME scenario (same
// upstream server, same egress policy / DB client) is dispatched through
// Engine A (this package's Scene/ExecProgram interpreter) and through
// Engine B (blue-runtime-go via internal/bluehost), and the two engines'
// observable outcomes are compared primitive by primitive. A mismatch
// fails the test naming the primitive (issue #358 §4/§7).
//
// Scope: the primitives ENGINE-B-PARITY-ORION actually wires a host for —
// http.request execute dispatch and db.query execute dispatch. Gate/delay/
// await/on-tick/on-call/on-platform-event are Engine B-internal (no host
// transport to differentially compare against Engine A's OWN distinct
// mechanisms — see bluehost/host_entrypoints_test.go and effects_test.go
// for their single-engine proofs) — Engine A doesn't expose an equivalent
// synchronous harness for those without standing up a full Show/Scene
// production wiring, out of scope for this differential slice.

func canonicalJSONForTest(value any) []byte {
	// Mirrors bluehost's own canonicalJSON test helper (duplicated rather
	// than exported across an internal package boundary neither owns).
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := []byte{'{'}
		for i, k := range keys {
			if i > 0 {
				out = append(out, ',')
			}
			kb, _ := json.Marshal(k)
			out = append(out, kb...)
			out = append(out, ':')
			out = append(out, canonicalJSONForTest(v[k])...)
		}
		return append(out, '}')
	case []any:
		out := []byte{'['}
		for i, c := range v {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, canonicalJSONForTest(c)...)
		}
		return append(out, ']')
	default:
		b, _ := json.Marshal(v)
		return b
	}
}

func digestOfForTest(value map[string]any) string {
	sum := sha256.Sum256(canonicalJSONForTest(value))
	return fmt.Sprintf("sha256:%x", sum)
}

// buildEngineBHTTPProgram is the Engine B analogue of the Engine A ExecProgram
// below: on-start -> req (core.http.request@1, url=targetURL literal) ->
// mark (core.variable.set@1 "result" bound to req.status), exposed as the
// declared state output "result".
func buildEngineBHTTPProgram(t *testing.T, targetURL string) []byte {
	t.Helper()
	execPort := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	dataPort := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": typ, "required": required}
	}
	program := map[string]any{
		"schema_version": "blue.program.v1", "program_id": "ab-parity-http",
		"compiler_version": "0.1.0", "runtime_abi": "blue-runtime-abi.v1",
		"runtime_module": map[string]any{"name": "blue-runtime-go", "version": "0.1.0"},
		"source_revision": map[string]any{
			"id": "ab-parity-http", "kind": "blueprint-version", "revision": 1,
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
			map[string]any{
				"id": "core.http.request@1", "kind": "control", "config": []any{},
				"inputs": []any{
					dataPort("body", "core.json", false), dataPort("headers", "core.json", false),
					dataPort("method", "core.json", false), dataPort("query", "core.json", false),
					dataPort("timeout_ms", "core.json", false), dataPort("url", "core.json", false),
					execPort("in"),
				},
				"outputs": []any{
					dataPort("body", "core.json", false), dataPort("headers", "core.json", false),
					dataPort("ok", "core.json", false), dataPort("status", "core.json", false),
					execPort("error"), execPort("then"),
				},
			},
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
			map[string]any{"id": "req", "opcode": "core.http.request@1", "config": map[string]any{}},
		},
		"entrypoints": []any{
			map[string]any{"id": "start", "kind": "start", "node_id": "entry", "port": "then"},
		},
		"exec_edges": []any{
			map[string]any{"from_node": "entry", "from_port": "then", "to_node": "req", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "req", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0},
		},
		"data_edges": []any{
			map[string]any{"from_node": "req", "from_port": "status", "to_node": "mark", "to_port": "value"},
		},
		"data_literals": []any{
			map[string]any{"node_id": "req", "port": "url", "value": targetURL},
		},
		"effects": []any{}, "requires": []any{}, "topics": []any{}, "timers": []any{},
		"state": map[string]any{
			"variables": []any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
			"outputs":   []any{map[string]any{"name": "result", "type": "core.json"}},
		},
	}
	program["program_digest"] = digestOfForTest(program)
	out, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal fixture program: %v", err)
	}
	return out
}

// TestEngineABParity_HTTPRequestExecuteSameServerSameStatus dispatches the
// SAME http.request against the SAME httptest server through Engine A
// (execHTTPRequest) and Engine B (bluehost.NewEffectHandlers), bound to
// the SAME *effects.EgressPolicy instance, and asserts they observe the
// identical status code — the on-air/execute half of ENGINE-B-PARITY-
// ORION's differential proof (issue #358 §4).
func TestEngineABParity_HTTPRequestExecuteSameServerSameStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	egress := effects.NewEgressPolicy([]string{u.Hostname()}, true).InsecureAllowPrivateForTest()

	// --- Engine A ---
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `"`)},
				Next:   map[string]ExecTarget{"then": {Node: "set.status"}, "error": {Node: "set.err"}}},
			"set.status": setFromPin("set.status", "status", "req", "status", nil),
			"set.err":    setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "req"}}},
	}
	eff := &SceneEffects{Runner: newTestRunner(t), Egress: egress}
	sc := effectsScene(t, "ab-parity-http", prog, eff)
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.status", `201`, 2*time.Second)
	engineAStatus, _ := sc.state.Get("__vars.bp.status")

	// --- Engine B ---
	handlers := bluehost.NewEffectHandlers(bluehost.EffectDeps{Egress: egress}, blueruntime.Execute)
	program := buildEngineBHTTPProgram(t, srv.URL)
	rt := blueruntime.NewRuntime()
	handle, err := rt.Load(program)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	instance, err := rt.Start(handle, blueruntime.StartOptions{InstanceID: "ab-parity", Mode: blueruntime.Execute, EffectHandlers: handlers})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := rt.Step(instance); err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	step, err := rt.Step(instance)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	engineBStatus, _ := step.Outputs["result"].(json.Number)

	if string(engineAStatus) != engineBStatus.String() {
		t.Fatalf("http.request status diverges: Engine A=%s Engine B=%s", engineAStatus, engineBStatus)
	}
}

// TestEngineABParity_HTTPRequestPreviewNoNetworkOnEngineB keeps a direct
// handler-level guard for Blue's synthetic preview response. The differential
// Engine-A PreviewSlot comparison, including the no-network assertion, lives
// in the inventory transport test where both paths are exercised together.
func TestEngineABParity_HTTPRequestPreviewNoNetworkOnEngineB(t *testing.T) {
	poison := effects.NewEgressPolicy(nil, false)
	handlers := bluehost.NewEffectHandlers(bluehost.EffectDeps{Egress: poison}, blueruntime.Preview)
	program := buildEngineBHTTPProgram(t, "https://example.invalid/never-dialed")
	rt := blueruntime.NewRuntime()
	handle, err := rt.Load(program)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	instance, err := rt.Start(handle, blueruntime.StartOptions{InstanceID: "ab-parity-preview", Mode: blueruntime.Preview, EffectHandlers: handlers})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := rt.Step(instance); err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	step, err := rt.Step(instance)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if v, _ := step.Outputs["result"].(json.Number); v != "0" {
		t.Fatalf("expected preview status 0 (no dispatch), got %#v", step.Outputs["result"])
	}
}
