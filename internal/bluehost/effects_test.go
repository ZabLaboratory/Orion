package bluehost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// --- canonical digest (mirrors blueruntime's unexported canonical.go, so a
// hand-built program map can carry a program_digest ParseProgram accepts —
// no exported constructor exists in the module for building a NEW program
// from scratch, only for parsing one). Numbers in this test's fixtures are
// always small non-negative integers, so plain fmt formatting matches
// blueruntime's json.Number-based formatNumber byte-for-byte.

func canonicalJSON(value any) []byte {
	var buf bytes.Buffer
	writeCanonicalForTest(&buf, value)
	return buf.Bytes()
}

func writeCanonicalForTest(buf *bytes.Buffer, value any) {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		b, _ := json.Marshal(v)
		buf.Write(b)
	case int:
		fmt.Fprintf(buf, "%d", v)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			writeCanonicalForTest(buf, v[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, c := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalForTest(buf, c)
		}
		buf.WriteByte(']')
	default:
		panic(fmt.Sprintf("writeCanonicalForTest: unsupported %T", value))
	}
}

func digestOf(value map[string]any) string {
	sum := sha256.Sum256(canonicalJSON(value))
	return fmt.Sprintf("sha256:%x", sum)
}

// buildHTTPRequestProgram assembles a minimal blue.program.v1 exercising
// `core.http.request@1` as an opcode of full right (ENGINE-B-PARITY-BLUE):
// on-start -> req (http.request, url=targetURL literal) -> mark
// (variable.set "result" from req.status) -> declared state output
// "result", so StepResult.Outputs["result"] observes the dispatch outcome
// without any var-get plumbing.
func buildHTTPRequestProgram(t *testing.T, targetURL string) []byte {
	t.Helper()
	execPort := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	dataPort := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": typ, "required": required}
	}
	program := map[string]any{
		"schema_version":   "blue.program.v1",
		"program_id":       "fixture-http-full-right",
		"compiler_version": "0.1.0",
		"runtime_abi":      "blue-runtime-abi.v1",
		"runtime_module":   map[string]any{"name": "blue-runtime-go", "version": "0.1.0"},
		"source_revision": map[string]any{
			"id": "fixture-http-full-right", "kind": "blueprint-version", "revision": 1,
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
			map[string]any{
				"id": "core.event.on-start@1", "kind": "entrypoint",
				"config": []any{}, "inputs": []any{},
				"outputs": []any{execPort("then")},
			},
			map[string]any{
				"id": "core.http.request@1", "kind": "control",
				"config": []any{},
				"inputs": []any{
					dataPort("body", "core.json", false),
					dataPort("headers", "core.json", false),
					dataPort("method", "core.json", false),
					dataPort("query", "core.json", false),
					dataPort("timeout_ms", "core.json", false),
					dataPort("url", "core.json", false),
					execPort("in"),
				},
				"outputs": []any{
					dataPort("body", "core.json", false),
					dataPort("headers", "core.json", false),
					dataPort("ok", "core.json", false),
					dataPort("status", "core.json", false),
					execPort("error"),
					execPort("then"),
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
		"effects":  []any{},
		"requires": []any{},
		"topics":   []any{},
		"timers":   []any{},
		"state": map[string]any{
			"variables": []any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
			"outputs":   []any{map[string]any{"name": "result", "type": "core.json"}},
		},
	}
	program["program_digest"] = digestOf(program)
	out, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal fixture program: %v", err)
	}
	return out
}

func startedInstance(t *testing.T, program []byte, mode blueruntime.Mode, handlers map[string]blueruntime.EffectFunc) *blueruntime.InstanceHandle {
	t.Helper()
	rt := blueruntime.NewRuntime()
	handle, err := rt.Load(program)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	instance, err := rt.Start(handle, blueruntime.StartOptions{InstanceID: "eff-test", Mode: mode, EffectHandlers: handlers})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return instance
}

// TestEffectHandlers_PreviewNeverDialsNetwork proves issue #358 §7's
// explicit preview criterion by construction: the egress policy's
// allowlist is EMPTY and CheckURL/Client are never exercised (a spy
// RoundTripper would fail the test if dialed) — Preview returns the
// synthetic no-op result and fires `then`, never touching the network.
func TestEffectHandlers_PreviewNeverDialsNetwork(t *testing.T) {
	dialed := false
	// A deny-all policy whose lookup would flag any dial attempt — since
	// Preview must never even reach CheckURL/Client, this proves the point
	// by construction: a dialed lookup here means the no-op path regressed.
	poison := effects.NewEgressPolicy(nil, false)
	poison.SetLookupForTest(func(_ context.Context, _ string) ([]net.IPAddr, error) {
		dialed = true
		return nil, fmt.Errorf("spy: must never resolve in preview")
	})
	handlers := NewEffectHandlers(EffectDeps{Egress: poison}, blueruntime.Preview)
	program := buildHTTPRequestProgram(t, "https://example.invalid/should-never-be-dialed")
	instance := startedInstance(t, program, blueruntime.Preview, handlers)

	rt := blueruntime.NewRuntime()
	if _, err := rt.Step(instance); err != nil { // fires on-start
		t.Fatalf("Step (on-start): %v", err)
	}
	step, err := rt.Step(instance)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if dialed {
		t.Fatal("preview dialed the network")
	}
	if v, _ := step.Outputs["result"].(json.Number); v != "0" {
		t.Fatalf("expected preview status 0, got %#v (outputs=%#v)", step.Outputs["result"], step.Outputs)
	}
}

// TestEffectHandlers_ExecuteDispatchesRealHTTP proves the on-air path
// executes for real through the injected egress policy: a live httptest
// server is hit and its status code round-trips onto the declared state
// output.
func TestEffectHandlers_ExecuteDispatchesRealHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	egress := effects.NewEgressPolicy([]string{u.Hostname()}, true).InsecureAllowPrivateForTest()
	handlers := NewEffectHandlers(EffectDeps{Egress: egress}, blueruntime.Execute)
	program := buildHTTPRequestProgram(t, srv.URL)
	instance := startedInstance(t, program, blueruntime.Execute, handlers)

	rt := blueruntime.NewRuntime()
	if _, err := rt.Step(instance); err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	step, err := rt.Step(instance)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if v, _ := step.Outputs["result"].(json.Number); v != "201" {
		t.Fatalf("expected status 201, got %#v (outputs=%#v)", step.Outputs["result"], step.Outputs)
	}
}

// TestEffectHandlers_ExecuteEgressDeniedFiresErrorPin proves a denied host
// fails to the node's `error` pin (no `mark` write, since `mark` is wired
// off `then` only) rather than crashing the instance.
func TestEffectHandlers_ExecuteEgressDeniedFiresErrorPin(t *testing.T) {
	egress := effects.NewEgressPolicy(nil, false) // deny-all
	handlers := NewEffectHandlers(EffectDeps{Egress: egress}, blueruntime.Execute)
	program := buildHTTPRequestProgram(t, "https://not-allowlisted.example.com/x")
	instance := startedInstance(t, program, blueruntime.Execute, handlers)

	rt := blueruntime.NewRuntime()
	if _, err := rt.Step(instance); err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	step, err := rt.Step(instance)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if v := step.Outputs["result"]; v != nil {
		t.Fatalf("error pin must not reach `mark` (then-only wiring): outputs=%#v", step.Outputs)
	}
}

// TestEffectHandlers_ExecuteWithoutEgressPolicyFailsClosed mirrors Engine
// A's fail-closed posture (SceneEffects.Egress nil = deny-all) — an
// unconfigured Host still never crashes, never silently succeeds.
func TestEffectHandlers_ExecuteWithoutEgressPolicyFailsClosed(t *testing.T) {
	handlers := NewEffectHandlers(EffectDeps{}, blueruntime.Execute)
	program := buildHTTPRequestProgram(t, "https://example.invalid/x")
	instance := startedInstance(t, program, blueruntime.Execute, handlers)

	rt := blueruntime.NewRuntime()
	if _, err := rt.Step(instance); err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	step, err := rt.Step(instance)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if v := step.Outputs["result"]; v != nil {
		t.Fatalf("unconfigured egress must fail closed to `error`, not `then`: outputs=%#v", step.Outputs)
	}
}
