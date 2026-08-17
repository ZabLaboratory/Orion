// External test package: internal/providers imports internal/bluehost
// (events.go's InjectActive), so a same-package test that also imports
// internal/providers would be an import cycle. Black-box testing through
// the public Host API only — no different in practice, Host has no
// unexported surface this test needs.
package bluehost_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
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
	h.SetHTTPEffects(bluehost.EffectDeps{Egress: egress, Runner: runner}, slog.Default())

	program := buildHTTPEffectProgram(t, server.URL)
	if err := h.Take("http-effect-cycle", "scene-1", "sha256:http-effect-cycle", program, providers.Registry(), providers.Policy(true), nil); err != nil {
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

// TestHost_HTTPEffectStaleCompletionIsDroppedAfterTake proves that an
// asynchronous completion from the outgoing on-air instance cannot be
// delivered to the replacement instance. The first request is held open
// across Take; only the replacement's response is allowed to reach the
// completion entrypoint.
func TestHost_HTTPEffectStaleCompletionIsDroppedAfterTake(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			close(firstStarted)
			<-releaseFirst
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"path":"`+r.URL.Path+`"}`)
	}))
	defer server.Close()

	serverHost := server.Listener.Addr().(*net.TCPAddr).IP.String()
	egress := effects.NewEgressPolicy([]string{serverHost}, true).InsecureAllowPrivateForTest()
	runner := effects.NewRunner(2, 8, slog.Default())
	runner.Start()
	defer runner.Stop()

	h := bluehost.NewHost()
	h.SetHTTPEffects(bluehost.EffectDeps{Egress: egress, Runner: runner}, slog.Default())
	firstURL := server.URL + "/first"
	secondURL := server.URL + "/second"
	if err := h.Take("stale-first", "scene-1", "sha256:stale-first", buildHTTPEffectProgram(t, firstURL), providers.Registry(), providers.Policy(true), nil); err != nil {
		t.Fatalf("Take first: %v", err)
	}
	if _, err := h.Step(bluehost.SlotOnAir); err != nil {
		t.Fatalf("Step first: %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for outgoing instance request")
	}

	if err := h.Take("stale-second", "scene-1", "sha256:stale-second", buildHTTPEffectProgram(t, secondURL), providers.Registry(), providers.Policy(true), nil); err != nil {
		t.Fatalf("Take replacement: %v", err)
	}
	if _, err := h.Step(bluehost.SlotOnAir); err != nil {
		t.Fatalf("Step replacement: %v", err)
	}
	close(releaseFirst)

	deadline := time.Now().Add(5 * time.Second)
	var result map[string]any
	for time.Now().Before(deadline) {
		step, err := h.Step(bluehost.SlotOnAir)
		if err != nil {
			t.Fatalf("Step completion: %v", err)
		}
		if value, ok := step.Variables["result"].(map[string]any); ok {
			result = value
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if result == nil {
		t.Fatal("timed out waiting for replacement completion")
	}
	response, ok := result["response"].(map[string]any)
	if !ok {
		t.Fatalf("replacement completion response=%#v", result["response"])
	}
	body, ok := response["body"].(map[string]any)
	if !ok || body["path"] != "/second" {
		t.Fatalf("stale completion overwrote replacement: response body=%#v", response["body"])
	}
}

// customHTTPProviderWithoutPreviewNoop is httpRequestProvider()
// (internal/providers/providers.go) with its "preview" hint flipped from
// "noop" to "emulated". Production's real Prepare/Take path always passes
// providers.Registry(), whose core.http.request descriptor already declares
// "preview":"noop" — meaning blueruntime's OWN walker (fireEffectInvoke's
// previewNoop branch) already short-circuits this exact fixture's
// invocation before Host.dispatchInvocations ever sees it, independent of
// the Host-level gate this test exists to prove. Passing this alternate
// descriptor instead isolates the seam ORION-PREVIEW-EFFECT-GATE actually
// fixed: dispatchInvocations' own mode check, not an upstream Blue
// declaration this test would otherwise depend on staying in sync.
func customHTTPProviderWithoutPreviewNoop() []map[string]any {
	return []map[string]any{
		{
			"schema_version": "blue.capability-provider.v1",
			"capability":     "core.http.request",
			"version":        "1",
			"health":         "healthy",
			"operations": []any{
				map[string]any{
					"name":          "request",
					"request_type":  "core.json",
					"response_type": "core.json",
					"preview":       "emulated",
					"execute":       "allowed",
					"idempotency":   "unsupported",
					"cancellation":  "unsupported",
					"deadline":      "unsupported",
					"limits": map[string]any{
						"max_in_flight":     json.Number("8"),
						"max_payload_bytes": json.Number("1048576"),
						"max_deadline_ms":   json.Number("0"),
					},
					"backpressure": "reject",
					"error_codes":  []any{"EGRESS_BLOCKED", "PROVIDER_FAILED"},
				},
			},
		},
	}
}

// TestHost_PreviewHTTPEffectGateBlocksRealDispatch (ORION-PREVIEW-EFFECT-
// GATE) proves dispatchInvocations' mode gate, not blueruntime's own
// preview-noop admission, is what stops a preview instance's
// core.effect.invoke@1/core.http.request from reaching the real network:
// the invocation IS generated by the walker (asserted below via
// StepResult.Invocations, using customHTTPProviderWithoutPreviewNoop so the
// walker-level short-circuit does not apply) and IS wired to a real egress
// policy and worker pool identical to TestHost_HTTPEffectFullCycle's — the
// ONLY difference from that passing antenna test is the slot. If the gate
// were removed, this test would dial server and go red (verified by hand:
// temporarily removing the `if modeFor(slot) != blueruntime.Execute`
// early-return in dispatchInvocations turns this red; restoring it turns it
// green — the CI diff itself never contains that revert).
func TestHost_PreviewHTTPEffectGateBlocksRealDispatch(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	serverHost := server.Listener.Addr().(*net.TCPAddr).IP.String()
	egress := effects.NewEgressPolicy([]string{serverHost}, true).InsecureAllowPrivateForTest()
	runner := effects.NewRunner(2, 8, slog.Default())
	runner.Start()
	defer runner.Stop()

	h := bluehost.NewHost()
	h.SetHTTPEffects(bluehost.EffectDeps{Egress: egress, Runner: runner}, slog.Default())

	program := buildHTTPEffectProgram(t, server.URL)
	if err := h.Prepare(bluehost.SlotPreview, "preview-http-effect", "preview-http-effect-scene",
		"sha256:preview-http-effect", program, customHTTPProviderWithoutPreviewNoop(), providers.Policy(true), nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	step, err := h.Step(bluehost.SlotPreview)
	if err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	if len(step.Invocations) != 1 {
		t.Fatalf("on-start invocations = %d, want 1 (walker-level short-circuit engaged — this test is not exercising the Host gate)", len(step.Invocations))
	}

	// A REAL dispatch (TestHost_HTTPEffectFullCycle, identical server and
	// program) completes well under 5s. 500ms with no completion and no
	// server hit is conclusive that nothing was ever submitted.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		step, err := h.Step(bluehost.SlotPreview)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if _, ok := step.Variables["result"].(map[string]any); ok {
			t.Fatal("preview instance received an HTTP effect completion — it dialed the network")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("preview instance's core.http.request invocation reached the real server: %d hit(s)", got)
	}
}

// TestHost_HTTPEffectQueueFullOnAntennaAfterPreviewGate proves the antenna's
// back-pressure path (EFFECT_QUEUE_FULL, effect_http.go:104) still fires
// after ORION-PREVIEW-EFFECT-GATE — the actual risk Bastion named for this
// change is not preview leaking onto the network, it is a misplaced
// modeFor(slot) check silently starving the antenna (e.g. gating too much,
// or gating after the wrong return). The queue's one slot is pre-filled
// directly (never Start()ed, so nothing drains it) before Take/Step, forcing
// dispatchInvocations' Submit to fail deterministically — no network, no
// timing race.
func TestHost_HTTPEffectQueueFullOnAntennaAfterPreviewGate(t *testing.T) {
	egress := effects.NewEgressPolicy([]string{"example.invalid"}, true).InsecureAllowPrivateForTest()
	runner := effects.NewRunner(1, 1, slog.Default())
	filled := runner.Submit(effects.Job{
		Run:     func(context.Context) effects.Result { return effects.Result{} },
		Deliver: func(effects.Result) {},
	})
	if !filled {
		t.Fatal("failed to pre-fill the runner's sole queue slot")
	}

	h := bluehost.NewHost()
	h.SetHTTPEffects(bluehost.EffectDeps{Egress: egress, Runner: runner}, slog.Default())
	program := buildHTTPEffectProgram(t, "https://unreachable.invalid/effect")
	if err := h.Take("antenna-queue-full", "scene-1", "sha256:antenna-queue-full", program, providers.Registry(), providers.Policy(true), nil); err != nil {
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
		t.Fatal("timed out waiting for the queue-full completion")
	}
	if status, _ := result["status"].(string); status != "failed" {
		t.Fatalf("unexpected completion status: %#v", result["status"])
	}
	failure, ok := result["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing completion error: %#v", result["error"])
	}
	if message, _ := failure["message"].(string); message != "EFFECT_QUEUE_FULL" {
		t.Fatalf("unexpected completion error message: %#v", failure["message"])
	}
}

func TestHost_HTTPEffectMissingDependenciesCompletesFailure(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*bluehost.Host, *testing.T)
	}{
		{name: "bundle absent"},
		{
			name: "runner absent",
			configure: func(h *bluehost.Host, _ *testing.T) {
				egress := effects.NewEgressPolicy([]string{"example.invalid"}, false)
				h.SetHTTPEffects(bluehost.EffectDeps{Egress: egress}, slog.Default())
			},
		},
		{
			name: "egress absent",
			configure: func(h *bluehost.Host, t *testing.T) {
				runner := effects.NewRunner(1, 1, slog.Default())
				runner.Start()
				t.Cleanup(runner.Stop)
				h.SetHTTPEffects(bluehost.EffectDeps{Runner: runner}, slog.Default())
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := bluehost.NewHost()
			if tc.configure != nil {
				tc.configure(h, t)
			}
			program := buildHTTPEffectProgram(t, "https://unreachable.invalid/effect")
			if err := h.Take("missing-http-deps", "scene-1", "sha256:missing-http-deps", program, providers.Registry(), providers.Policy(true), nil); err != nil {
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
				t.Fatal("timed out waiting for the provider-unavailable completion")
			}
			if status, _ := result["status"].(string); status != "failed" {
				t.Fatalf("unexpected completion status: %#v", result["status"])
			}
			failure, ok := result["error"].(map[string]any)
			if !ok {
				t.Fatalf("missing completion error: %#v", result["error"])
			}
			if code, _ := failure["code"].(string); code != "PROVIDER_FAILED" {
				t.Fatalf("unexpected completion error code: %#v", failure["code"])
			}
			if message, _ := failure["message"].(string); !strings.Contains(message, "EFFECT_PROVIDER_UNAVAILABLE") {
				t.Fatalf("missing provider-unavailable message: %#v", failure["message"])
			}
		})
	}
}
