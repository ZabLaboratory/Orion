package bluehost

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync/atomic"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
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

func buildDirectHTTPEntrypointProgram(t *testing.T, kind, targetURL, leaf string) []byte {
	t.Helper()
	execPort := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	dataPort := func(name string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": "core.json", "required": required}
	}
	var entryOpcode string
	entrypoint := map[string]any{"id": "trigger", "kind": kind, "node_id": "trigger-node", "port": "then"}
	if kind == "tick" {
		entryOpcode = "core.event.on-tick@1"
		entrypoint["id"] = "tick"
	} else if kind == "call" {
		entryOpcode = "core.operator.on-call@1"
		entrypoint["id"] = "arm"
	} else {
		entryOpcode = "core.event.on-platform-event@1"
		entrypoint["id"] = "platform"
		entrypoint["leaf"] = leaf
	}
	var entryOutputs []any
	if kind == "tick" {
		entryOutputs = []any{dataPort("delta_seconds", false), execPort("then")}
	} else {
		entryOutputs = []any{dataPort("payload", false), execPort("then")}
	}
	program := map[string]any{
		"schema_version":   blueruntime.ProgramSchema,
		"program_id":       "host-entrypoint-http-" + kind,
		"compiler_version": blueruntime.CompilerVersion,
		"runtime_abi":      blueruntime.RuntimeABI,
		"runtime_module":   map[string]any{"name": blueruntime.RuntimeModule, "version": blueruntime.RuntimeModuleVer},
		"source_revision": map[string]any{
			"id": "host-entrypoint-http-" + kind, "kind": "blueprint-version", "revision": 1,
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
			"max_steps_per_dispatch": 32, "max_queue_depth": 32, "max_execution_ms": 1000,
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
				"id": "core.http.request@1", "kind": "control", "config": []any{},
				"inputs": []any{
					dataPort("body", false), dataPort("headers", false), dataPort("method", false),
					dataPort("query", false), dataPort("timeout_ms", false), dataPort("url", false), execPort("in"),
				},
				"outputs": []any{
					dataPort("body", false), dataPort("headers", false), dataPort("ok", false), dataPort("status", false),
					execPort("error"), execPort("then"),
				},
			},
			map[string]any{
				"id": "core.variable.set@1", "kind": "pure",
				"config":  []any{map[string]any{"name": "variable", "kind": "data", "type": "core.string", "required": true}},
				"inputs":  []any{dataPort("value", true), execPort("in")},
				"outputs": []any{dataPort("value", false), execPort("then")},
			},
		},
		"nodes": []any{
			map[string]any{"id": "body", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "body"}},
			map[string]any{"id": "error", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "error"}},
			map[string]any{"id": "entry", "opcode": "core.event.on-start@1", "config": map[string]any{}},
			map[string]any{"id": "req", "opcode": "core.http.request@1", "config": map[string]any{}},
			map[string]any{"id": "status", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "status"}},
			map[string]any{"id": "trigger-node", "opcode": entryOpcode, "config": map[string]any{}},
		},
		"entrypoints": []any{
			map[string]any{"id": "start", "kind": "start", "node_id": "entry", "port": "then"},
			entrypoint,
		},
		"exec_edges": []any{
			map[string]any{"from_node": "req", "from_port": "error", "to_node": "error", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "req", "from_port": "then", "to_node": "status", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "status", "from_port": "then", "to_node": "body", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "trigger-node", "from_port": "then", "to_node": "req", "to_port": "in", "sequence": 0},
		},
		"data_edges": []any{
			map[string]any{"from_node": "req", "from_port": "body", "to_node": "body", "to_port": "value"},
			map[string]any{"from_node": "req", "from_port": "status", "to_node": "status", "to_port": "value"},
		},
		"data_literals": []any{
			map[string]any{"node_id": "error", "port": "value", "value": "error"},
			map[string]any{"node_id": "req", "port": "url", "value": targetURL},
		},
		"effects": []any{}, "requires": []any{}, "topics": []any{}, "timers": []any{},
		"state": map[string]any{
			"variables": []any{
				map[string]any{"name": "body", "type": "core.json", "initial": nil},
				map[string]any{"name": "error", "type": "core.json", "initial": nil},
				map[string]any{"name": "status", "type": "core.json", "initial": nil},
			},
			"outputs": []any{map[string]any{"name": "body", "type": "core.json"}, map[string]any{"name": "status", "type": "core.json"}},
		},
	}
	sortByID(program["opcodes"].([]any))
	sortByID(program["nodes"].([]any))
	sortByID(program["entrypoints"].([]any))
	program["program_digest"] = digestOf(program)
	out, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal direct entrypoint HTTP fixture: %v", err)
	}
	return out
}

// TestHost_EntryPointsUseDirectEffectHandlersBecauseBlueDoesNotReturnInvocations
// proves the current Blue ABI honestly: Tick, Call and WritePlatformEvent
// execute a real core.http.request@1 through StartOptions.EffectHandlers and
// expose its status/body continuation, but runEntrypointsWithOutputs does not
// return core.effect.invoke@1 envelopes in StepResult.Invocations. Host's
// generic invocation dispatch is therefore defensive for future/runtime paths;
// this test does not claim async invocation parity for these three entrypoints.
func TestHost_EntryPointsUseDirectEffectHandlersBecauseBlueDoesNotReturnInvocations(t *testing.T) {
	leaf := "__inputs.platform.twitch.channel_1.last_follow"
	cases := []struct {
		name string
		kind string
		call func(*Host) (blueruntime.StepResult, error)
	}{
		{name: "tick", kind: "tick", call: func(h *Host) (blueruntime.StepResult, error) { return h.Tick(SlotOnAir, 0.25) }},
		{name: "call", kind: "call", call: func(h *Host) (blueruntime.StepResult, error) {
			return h.Call(SlotOnAir, "arm", map[string]any{"source": "test"})
		}},
		{name: "platform-event", kind: "platform-event", call: func(h *Host) (blueruntime.StepResult, error) {
			return h.WritePlatformEvent(SlotOnAir, leaf, map[string]any{"source": "test"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.URL.Path != "/entry/"+tc.name {
					t.Errorf("primitive=core.http.request@1 scenario=%s path=%q, want /entry/%s", tc.name, r.URL.Path, tc.name)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"entry":"` + tc.name + `"}`))
			}))
			defer server.Close()
			serverHost := server.Listener.Addr().(*net.TCPAddr).IP.String()
			egress := effects.NewEgressPolicy([]string{serverHost}, true).InsecureAllowPrivateForTest()
			h := NewHost()
			t.Cleanup(func() { _ = h.Release(SlotOnAir, "test-cleanup") })
			program := buildDirectHTTPEntrypointProgram(t, tc.kind, server.URL+"/entry/"+tc.name, leaf)
			digest := "sha256:entrypoint-" + tc.name
			handlers := NewEffectHandlers(EffectDeps{Egress: egress}, blueruntime.Execute)
			baseHTTPHandler := handlers["core.http.request@1"]
			var hostMuAccessible atomic.Bool
			handlers["core.http.request@1"] = func(config, inputs map[string]any) (map[string]any, error) {
				// Calling Digest from inside the synchronous handler proves that
				// Host's slot mutex is not held while handler I/O executes.
				if h.Digest(SlotOnAir) != digest {
					t.Errorf("primitive=core.http.request@1 scenario=%s handler observed unexpected slot digest", tc.name)
				}
				hostMuAccessible.Store(true)
				return baseHTTPHandler(config, inputs)
			}
			if err := h.Prepare(SlotOnAir, "entrypoint-"+tc.name, "sha256:entrypoint-"+tc.name, program, nil, nil,
				handlers); err != nil {
				t.Fatalf("primitive=core.http.request@1 scenario=%s Host.Prepare: %v", tc.name, err)
			}
			if _, err := h.Step(SlotOnAir); err != nil {
				t.Fatalf("primitive=core.event.%s scenario=%s start Step: %v", tc.kind, tc.name, err)
			}
			step, err := tc.call(h)
			if err != nil {
				t.Fatalf("primitive=core.event.%s scenario=%s entrypoint: %v", tc.kind, tc.name, err)
			}
			if len(step.Invocations) != 0 {
				t.Fatalf("primitive=core.event.%s scenario=%s Blue runtime unexpectedly returned async invocations=%#v; current ABI documents direct EffectHandlers only", tc.kind, tc.name, step.Invocations)
			}
			if got, ok := step.Variables["status"].(json.Number); !ok || got.String() != "200" {
				t.Fatalf("primitive=core.http.request@1 scenario=%s continuation status=%#v, want 200", tc.name, step.Variables["status"])
			}
			body, ok := step.Variables["body"].(map[string]any)
			if !ok || body["entry"] != tc.name {
				t.Fatalf("primitive=core.http.request@1 scenario=%s continuation body=%#v, want entry=%s", tc.name, step.Variables["body"], tc.name)
			}
			if value := step.Variables["error"]; value != nil {
				t.Fatalf("primitive=core.http.request@1 scenario=%s error continuation unexpectedly fired: %#v", tc.name, value)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("primitive=core.http.request@1 scenario=%s server hits=%d, want 1", tc.name, got)
			}
			if !hostMuAccessible.Load() {
				t.Fatalf("primitive=core.http.request@1 scenario=%s handler did not execute", tc.name)
			}
		})
	}
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
