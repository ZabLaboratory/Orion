// External test package: internal/providers imports internal/bluehost
// (events.go's InjectActive), so a same-package test that also imports
// internal/providers would be an import cycle. Black-box testing through
// the public Host API only — no different in practice, Host has no
// unexported surface this test needs.
package bluehost_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/providers"
)

// sortByField reorders items ([]any of map[string]any) in place by the
// string value under key — blueruntime.ParseProgram requires several
// arrays (types/id, opcodes/id, nodes/id, entrypoints/id, ...) in strict
// ascending canonical order and fails closed (PROGRAM_MALFORMED) otherwise.
func sortByField(items []any, key string) {
	sort.Slice(items, func(i, j int) bool {
		left, _ := items[i].(map[string]any)[key].(string)
		right, _ := items[j].(map[string]any)[key].(string)
		return left < right
	})
}

// buildHTTPEffectProgram loads the bluespike `core.http.request` requires
// fixture and extends it with a topic entrypoint that observes the
// effect's completion — proving the full async cycle, not just admission.
// It (a) points the http-call's `request` data literal at a real test
// server, (b) adds `core.event.on-event@1` (topic "effect.http.completed")
// -> `core.variable.set@1` so the completion's payload lands in a program
// output, and (c) recomputes program_digest over the mutated document via
// the same canonicalization blueruntime.ParseProgram verifies.
func buildHTTPEffectProgram(t *testing.T, serverURL string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../bluespike/testdata/02-http-requires.program.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var program map[string]any
	if err := dec.Decode(&program); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	dataLiterals, _ := program["data_literals"].([]any)
	found := false
	for _, raw := range dataLiterals {
		literal, ok := raw.(map[string]any)
		if !ok || literal["node_id"] != "http-call" {
			continue
		}
		literal["value"] = map[string]any{"url": serverURL, "method": "GET"}
		found = true
	}
	if !found {
		t.Fatal("fixture: http-call request data literal not found")
	}

	program["state"] = map[string]any{
		"outputs":   []any{map[string]any{"name": "result", "type": "core.json"}},
		"variables": []any{map[string]any{"name": "result", "initial": nil, "type": "core.json"}},
	}

	nodes, _ := program["nodes"].([]any)
	nodes = append(nodes,
		map[string]any{"id": "on-completed", "opcode": "core.event.on-event@1", "config": map[string]any{"topic": "effect.http.completed"}},
		map[string]any{"id": "set-result", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
	)
	sortByField(nodes, "id")
	program["nodes"] = nodes

	dataEdges, _ := program["data_edges"].([]any)
	dataEdges = append(dataEdges, map[string]any{
		"from_node": "on-completed", "from_port": "payload",
		"to_node": "set-result", "to_port": "value",
	})
	program["data_edges"] = dataEdges

	execEdges, _ := program["exec_edges"].([]any)
	execEdges = append(execEdges, map[string]any{
		"from_node": "on-completed", "from_port": "then", "sequence": json.Number("0"),
		"to_node": "set-result", "to_port": "in",
	})
	program["exec_edges"] = execEdges

	entrypoints, _ := program["entrypoints"].([]any)
	entrypoints = append(entrypoints, map[string]any{
		"id": "on-http-completed", "kind": "topic", "node_id": "on-completed",
		"port": "then", "topic": "effect.http.completed",
	})
	sortByField(entrypoints, "id")
	program["entrypoints"] = entrypoints

	opcodes, _ := program["opcodes"].([]any)
	opcodes = append(opcodes,
		map[string]any{
			"id": "core.event.on-event@1", "kind": "entrypoint",
			"config": []any{map[string]any{"kind": "data", "name": "topic", "required": true, "type": "core.string"}},
			"inputs": []any{},
			"outputs": []any{
				map[string]any{"kind": "data", "name": "payload", "required": true, "type": "core.json"},
				map[string]any{"kind": "exec", "name": "then", "required": true, "type": "core.exec"},
			},
		},
		map[string]any{
			"id": "core.variable.set@1", "kind": "control",
			"config": []any{map[string]any{"kind": "data", "name": "variable", "required": true, "type": "core.string"}},
			"inputs": []any{
				map[string]any{"kind": "data", "name": "value", "required": true, "type": "core.any"},
				map[string]any{"kind": "exec", "name": "in", "required": true, "type": "core.exec"},
			},
			"outputs": []any{
				map[string]any{"kind": "data", "name": "value", "required": true, "type": "core.any"},
				map[string]any{"kind": "exec", "name": "then", "required": true, "type": "core.exec"},
			},
		},
	)
	sortByField(opcodes, "id")
	program["opcodes"] = opcodes

	types, _ := program["types"].([]any)
	types = append(types, map[string]any{"id": "core.any", "kind": "primitive", "spec": map[string]any{"base": "any"}})
	sortByField(types, "id")
	program["types"] = types

	delete(program, "program_digest")
	digest, err := canonical.Digest(program)
	if err != nil {
		t.Fatalf("digest program: %v", err)
	}
	program["program_digest"] = digest

	data, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal program: %v", err)
	}
	return data
}

// TestHost_HTTPEffectFullCycle proves the complete async invocation cycle
// end to end through the Host API: a `core.effect.invoke@1` node emits a
// `core.http.request` invocation, Host executes it against a REAL local
// HTTP server (not a mock), reports the outcome via Runtime.Complete, and
// the program's own `on-event` node observes the completion on a later
// Step — the result lands in a program output, exactly the guarantee
// db.query/http.request already give on the legacy runtime.SceneEffects
// path (docs/runbooks/blue-primitives-log-harness.md §14), delivered here
// through the new bluehost/blueruntime mechanism instead.
func TestHost_HTTPEffectFullCycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"echo":"orion-336"}`))
	}))
	defer server.Close()

	serverHost := server.Listener.Addr().(*net.TCPAddr).IP.String()
	egress := effects.NewEgressPolicy([]string{serverHost}, true).InsecureAllowPrivateForTest()
	runner := effects.NewRunner(2, 8, slog.Default())
	runner.Start()
	defer runner.Stop()

	h := bluehost.NewHost()
	h.SetHTTPEffects(egress, runner, slog.Default())

	program := buildHTTPEffectProgram(t, server.URL)
	if err := h.Take("http-effect-cycle", "sha256:http-effect-cycle", program, providers.Registry(), providers.Policy(true), nil); err != nil {
		t.Fatalf("Take: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var result map[string]any
	for time.Now().Before(deadline) {
		step, err := h.Step(bluehost.SlotOnAir)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if value, ok := step.Variables["result"].(map[string]any); ok {
			result = value
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if result == nil {
		t.Fatal("timed out waiting for the effect completion to reach the program's on-event node")
	}

	// result is the FULL blue.effect.completion.v1 the on-event node's
	// `payload` output pin binds to (buildCompletionEvent wraps the whole
	// completion, not just the HTTP response) — assert both layers.
	if status, _ := result["status"].(string); status != "succeeded" {
		t.Fatalf("unexpected completion status: %#v", result["status"])
	}
	response, ok := result["response"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected completion response: %#v", result["response"])
	}
	status, ok := response["status"].(json.Number)
	if !ok || status.String() != "200" {
		t.Fatalf("unexpected response status: %#v", response["status"])
	}
	body, ok := response["body"].(map[string]any)
	if !ok || body["echo"] != "orion-336" {
		t.Fatalf("unexpected response body: %#v", response["body"])
	}
}
