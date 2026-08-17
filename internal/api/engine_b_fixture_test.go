package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Engine B fixtures for the operator rail's antenna leg
// (ORION-OPERATOR-RAIL-ENGINE-B, #335): a hand-built minimal blue.program.v1
// loaded onto bluehost.Host's SlotOnAir, exercised over the REAL HTTP router
// (mux.ServeHTTP), so a passing test proves operator/call, operator/resolve
// and cockpit/contracts reach a live Engine B instance — not a double.
//
// The canonical-digest helpers below mirror bluehost's own effects_test.go
// (canonicalJSON/digestOf) and host_entrypoints_test.go (sortByID): the
// blueruntime module exposes no constructor for building a NEW program from
// scratch, only for parsing one, and Load() strictly validates
// `program_digest` against its own canonicalization of the document. This is
// duplicated test-only code (already duplicated once, blueruntime →
// bluehost); a third copy here is the same low-risk trade the second one
// already made, not a new pattern.

func canonicalJSONForAPITest(value any) []byte {
	var buf bytes.Buffer
	writeCanonicalForAPITest(&buf, value)
	return buf.Bytes()
}

func writeCanonicalForAPITest(buf *bytes.Buffer, value any) {
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
			writeCanonicalForAPITest(buf, v[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, c := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalForAPITest(buf, c)
		}
		buf.WriteByte(']')
	default:
		panic(fmt.Sprintf("writeCanonicalForAPITest: unsupported %T", value))
	}
}

func digestOfForAPITest(value map[string]any) string {
	sum := sha256.Sum256(canonicalJSONForAPITest(value))
	return fmt.Sprintf("sha256:%x", sum)
}

// sortByIDForAPITest sorts a []any of map[string]any objects by their "id"
// field — blueruntime's program.go requires several top-level arrays
// (opcodes, entrypoints, nodes, …) in ascending "id" order.
func sortByIDForAPITest(items []any) {
	sort.Slice(items, func(i, j int) bool {
		a, _ := items[i].(map[string]any)["id"].(string)
		b, _ := items[j].(map[string]any)["id"].(string)
		return a < b
	})
}

// sortExecEdgesForAPITest / sortDataEdgesForAPITest apply program.go's exact
// canonical-order comparators (compareExecEdge: from_node, from_port,
// sequence, to_node, to_port; sortedComposite for data_edges: to_node,
// to_port, from_node, from_port) — required whenever an edge list has more
// than one entry (a single-edge list is trivially sorted).
func sortExecEdgesForAPITest(edges []any) {
	str := func(m map[string]any, key string) string { s, _ := m[key].(string); return s }
	num := func(m map[string]any, key string) int { n, _ := m[key].(int); return n }
	sort.Slice(edges, func(i, j int) bool {
		a, _ := edges[i].(map[string]any)
		b, _ := edges[j].(map[string]any)
		if str(a, "from_node") != str(b, "from_node") {
			return str(a, "from_node") < str(b, "from_node")
		}
		if str(a, "from_port") != str(b, "from_port") {
			return str(a, "from_port") < str(b, "from_port")
		}
		if num(a, "sequence") != num(b, "sequence") {
			return num(a, "sequence") < num(b, "sequence")
		}
		if str(a, "to_node") != str(b, "to_node") {
			return str(a, "to_node") < str(b, "to_node")
		}
		return str(a, "to_port") < str(b, "to_port")
	})
}

func sortDataEdgesForAPITest(edges []any) {
	str := func(m map[string]any, key string) string { s, _ := m[key].(string); return s }
	sort.Slice(edges, func(i, j int) bool {
		a, _ := edges[i].(map[string]any)
		b, _ := edges[j].(map[string]any)
		if str(a, "to_node") != str(b, "to_node") {
			return str(a, "to_node") < str(b, "to_node")
		}
		if str(a, "to_port") != str(b, "to_port") {
			return str(a, "to_port") < str(b, "to_port")
		}
		if str(a, "from_node") != str(b, "from_node") {
			return str(a, "from_node") < str(b, "from_node")
		}
		return str(a, "from_port") < str(b, "from_port")
	})
}

// buildEngineBOperatorProgram assembles a minimal blue.program.v1 hosting
// ONE on-call entrypoint (callID, writes its payload into __vars.<calledVar>,
// declared as a state output so a Tick(0) peek — see engineBFixture.peekVar —
// can read it back) and, when awaitName != "", ONE await-value node armed at
// on-start (writes its resolved value into __vars.<awaitedVar>, likewise a
// state output). Mirrors bluehost's own host_entrypoints_test.go
// (buildEntrypointProgram) for the call shape and blueruntime's
// conformance_matrix_test.go (TestParity_OperatorAwaitValue) for the
// await-value wiring (on-start -> await.in, await.then -> mark.in,
// await.value -> mark.value).
func buildEngineBOperatorProgram(t *testing.T, callID, calledVar, awaitName, awaitValueType, awaitedVar string) []byte {
	t.Helper()
	execPort := func(name string) map[string]any {
		return map[string]any{"name": name, "kind": "exec", "type": "core.exec", "required": true}
	}
	dataPort := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "kind": "data", "type": typ, "required": required}
	}

	opcodes := []any{
		map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{execPort("then")}},
		map[string]any{"id": "core.operator.on-call@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{},
			"outputs": []any{dataPort("payload", "core.json", false), execPort("then")}},
		map[string]any{
			"id": "core.variable.set@1", "kind": "pure",
			"config":  []any{dataPort("variable", "core.string", true)},
			"inputs":  []any{dataPort("value", "core.json", true), execPort("in")},
			"outputs": []any{dataPort("value", "core.json", false), execPort("then")},
		},
	}
	nodes := []any{
		map[string]any{"id": "start", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		map[string]any{"id": "call-entry", "opcode": "core.operator.on-call@1", "config": map[string]any{}},
		map[string]any{"id": "mark-called", "opcode": "core.variable.set@1", "config": map[string]any{"variable": calledVar}},
	}
	entrypoints := []any{
		map[string]any{"id": "start", "kind": "start", "node_id": "start", "port": "then"},
		map[string]any{"id": callID, "kind": "call", "node_id": "call-entry", "port": "then"},
	}
	execEdges := []any{
		map[string]any{"from_node": "call-entry", "from_port": "then", "to_node": "mark-called", "to_port": "in", "sequence": 0},
	}
	dataEdges := []any{
		map[string]any{"from_node": "call-entry", "from_port": "payload", "to_node": "mark-called", "to_port": "value"},
	}
	stateVars := []any{map[string]any{"name": calledVar, "type": "core.json", "initial": nil}}
	stateOutputs := []any{map[string]any{"name": calledVar, "type": "core.json"}}

	if awaitName != "" {
		opcodes = append(opcodes,
			// Canonical port order is data-before-exec (sortedComposite,
			// mirroring buildEntrypointProgram's entryOutputs convention).
			map[string]any{"id": "core.operator.await-value@1", "kind": "control",
				"config": []any{dataPort("await_name", "core.string", true), dataPort("value_type", "core.string", false)},
				"inputs": []any{execPort("in")}, "outputs": []any{dataPort("value", "core.json", false), execPort("then")}})
		nodes = append(nodes,
			map[string]any{"id": "await", "opcode": "core.operator.await-value@1",
				"config": map[string]any{"await_name": awaitName, "value_type": awaitValueType}},
			map[string]any{"id": "mark-awaited", "opcode": "core.variable.set@1", "config": map[string]any{"variable": awaitedVar}})
		execEdges = append(execEdges,
			map[string]any{"from_node": "start", "from_port": "then", "to_node": "await", "to_port": "in", "sequence": 0},
			map[string]any{"from_node": "await", "from_port": "then", "to_node": "mark-awaited", "to_port": "in", "sequence": 0})
		dataEdges = append(dataEdges,
			map[string]any{"from_node": "await", "from_port": "value", "to_node": "mark-awaited", "to_port": "value"})
		stateVars = append(stateVars, map[string]any{"name": awaitedVar, "type": "core.json", "initial": nil})
		stateOutputs = append(stateOutputs, map[string]any{"name": awaitedVar, "type": "core.json"})
	}

	program := map[string]any{
		"schema_version":   "blue.program.v1",
		"program_id":       "fixture-engine-b-" + callID,
		"compiler_version": "0.1.0",
		"runtime_abi":      "blue-runtime-abi.v1",
		"runtime_module":   map[string]any{"name": "blue-runtime-go", "version": "0.1.0"},
		"source_revision": map[string]any{
			"id": "fixture-engine-b-" + callID, "kind": "blueprint-version", "revision": 1,
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
		"data_literals": []any{},
		"effects":       []any{},
		"requires":      []any{},
		"topics":        []any{},
		"timers":        []any{},
		"state": map[string]any{
			"variables": stateVars,
			"outputs":   stateOutputs,
		},
	}
	sortByIDForAPITest(program["opcodes"].([]any))
	sortByIDForAPITest(program["entrypoints"].([]any))
	sortByIDForAPITest(program["nodes"].([]any))
	sortExecEdgesForAPITest(program["exec_edges"].([]any))
	sortDataEdgesForAPITest(program["data_edges"].([]any))
	program["program_digest"] = digestOfForAPITest(program)
	out, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal fixture program: %v", err)
	}
	return out
}

// engineBFixture wires a bluehost.Host with program loaded onto SlotOnAir
// (Take + the initial on-start Step, matching bluehost's own
// TestHost_CallFiresOnCallEntrypoint convention) behind the real
// RegisterPublic router — Show is present but empty (RegisterPublic and
// getCockpitContracts read deps.Show unconditionally, e.g.
// StreamRuleScenes()) so it must be non-nil, never active.
type engineBFixture struct {
	mux  *http.ServeMux
	host *bluehost.Host
}

func newEngineBOperatorFixture(t *testing.T, program []byte) *engineBFixture {
	t.Helper()
	host := bluehost.NewHost()
	if err := host.Take("engine-b-fixture", "sha256:engine-b-fixture", program, nil, nil, nil); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if _, err := host.Step(bluehost.SlotOnAir); err != nil {
		t.Fatalf("Step (on-start): %v", err)
	}
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	t.Cleanup(show.Stop)
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger: testLogger(), Metrics: m, Show: show,
		SceneIntent: &SceneIntentDeps{Host: host},
	})
	return &engineBFixture{mux: mux, host: host}
}

// newEngineBOperatorFixtureOnSlot mirrors newEngineBOperatorFixture but
// loads program onto slot (bluehost.SlotOnAir via Take, bluehost.SlotPreview
// via Prepare — the same two entry points production uses, take-on-air vs
// prepare-preview, scene_intent.go) instead of hardcoding SlotOnAir. Used by
// the ?target=preview leg of the operator tests
// (ORION-OPERATOR-PREVIEW-ENGINE-B).
func newEngineBOperatorFixtureOnSlot(t *testing.T, slot bluehost.Slot, program []byte) *engineBFixture {
	t.Helper()
	host := bluehost.NewHost()
	if err := loadEngineBSlot(host, slot, program); err != nil {
		t.Fatalf("load %s: %v", slot, err)
	}
	if _, err := host.Step(slot); err != nil {
		t.Fatalf("Step (on-start) %s: %v", slot, err)
	}
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	t.Cleanup(show.Stop)
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{
		Logger: testLogger(), Metrics: m, Show: show,
		SceneIntent: &SceneIntentDeps{Host: host},
	})
	return &engineBFixture{mux: mux, host: host}
}

// takeSlot additionally loads program onto slot on an already-built
// fixture — used to populate BOTH bluehost.Host slots with distinguishable
// programs for the operator rail's isolation proofs (a program prepared in
// SlotPreview must never be reachable without ?target=preview, and a
// program on SlotOnAir must never be reachable WITH it).
func (f *engineBFixture) takeSlot(t *testing.T, slot bluehost.Slot, program []byte) {
	t.Helper()
	if err := loadEngineBSlot(f.host, slot, program); err != nil {
		t.Fatalf("load %s: %v", slot, err)
	}
	if _, err := f.host.Step(slot); err != nil {
		t.Fatalf("Step (on-start) %s: %v", slot, err)
	}
}

// loadEngineBSlot is the slot-dispatch Take/Prepare share: SlotOnAir always
// goes through Take (atomic swap semantics, matching the take-on-air
// action), every other slot through Prepare (matching prepare-preview).
func loadEngineBSlot(host *bluehost.Host, slot bluehost.Slot, program []byte) error {
	if slot == bluehost.SlotOnAir {
		return host.Take("engine-b-fixture-onair", "sha256:engine-b-fixture-onair", program, nil, nil, nil)
	}
	return host.Prepare(slot, "engine-b-fixture-preview", "engine-b-fixture-preview-scene",
		"sha256:engine-b-fixture-preview", program, nil, nil, nil)
}

// peekVarSlot is peekVar generalised to an arbitrary slot (peekVar itself is
// left untouched — SlotOnAir-only, matching every existing caller).
func (f *engineBFixture) peekVarSlot(t *testing.T, slot bluehost.Slot, name string) any {
	t.Helper()
	result, err := f.host.Tick(slot, 0)
	if err != nil {
		t.Fatalf("Tick(0) peek %s: %v", slot, err)
	}
	return result.Outputs[name]
}

// peekVar reads back a declared state output of the on-air instance via a
// harmless Tick(0) (no on-tick entrypoint is declared in
// buildEngineBOperatorProgram's fixtures, so this fires nothing — it only
// re-reads runtime.outputs(instance), exactly what Call/Resolve's own
// StepResult already carried internally but the HTTP response never echoes,
// by design — see postOperatorCallEngineB's doc).
func (f *engineBFixture) peekVar(t *testing.T, name string) any {
	t.Helper()
	result, err := f.host.Tick(bluehost.SlotOnAir, 0)
	if err != nil {
		t.Fatalf("Tick(0) peek: %v", err)
	}
	return result.Outputs[name]
}

// noopPreviewWire is a minimal runtime.PreviewWire fake — the persistent
// LSDP wire is irrelevant to the HTTP-level assertions these tests make,
// only PreviewSlot's own state (Current()) is. Mirrors the equivalent fake
// internal/runtime's own tests use (parityPreviewWire, unexported there —
// can't be imported cross-package, so this is a second minimal copy).
type noopPreviewWire struct{}

func (noopPreviewWire) MirrorFor(string, string, *compiler.RenderBundle) runtime.SceneMirror {
	return nil
}
func (noopPreviewWire) SetActive(string) {}
func (noopPreviewWire) Drop(string)      {}
