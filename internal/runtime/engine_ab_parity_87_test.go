package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/conformance"
)

// TestEngineABParity_All87DedicatedBehavior is the executable master
// criterion for Orion#358. It deliberately creates one subtest per manifest
// row and dispatches by the way Orion serves that row. The subtest is only
// marked green after both Engine A and Engine B have crossed the same
// observable boundary (value, continuation, event payload, effect result,
// bound leaf, or platform-family payload).
func TestEngineABParity_All87DedicatedBehavior(t *testing.T) {
	manifest := conformance.Manifest()
	if got, want := len(manifest), 87; got != want {
		t.Fatalf("Engine A/B parity matrix manifest rows=%d, want %d", got, want)
	}

	seen := make(map[string]struct{}, len(manifest))
	for _, entry := range manifest {
		if _, duplicate := seen[entry.NodeID]; duplicate {
			t.Fatalf("manifest contains duplicate primitive %q", entry.NodeID)
		}
		seen[entry.NodeID] = struct{}{}
		served, ok := conformance.Classify(entry.NodeID)
		if !ok {
			t.Fatalf("primitive=%s has no Orion serving classification", entry.NodeID)
		}

		t.Run(entry.NodeID, func(t *testing.T) {
			switch served.Kind {
			case conformance.KindCompute:
				parity87RunCompute(t, entry.NodeID)
			case conformance.KindExecOp:
				parity87RunExecOp(t, entry.NodeID, served.Op)
			case conformance.KindEntry:
				parity87RunEntry(t, entry.NodeID)
			case conformance.KindLeafBound:
				parity87RunLeaf(t, entry.NodeID)
			case conformance.KindPlatformBound:
				parity87RunPlatformFamily(t, entry.NodeID)
			default:
				t.Fatalf("primitive=%s has unsupported serving kind %q", entry.NodeID, served.Kind)
			}
			t.Logf("ENGINE_AB_PARITY primitive=%s kind=%s PASS", entry.NodeID, served.Kind)
		})
	}
	if got := len(seen); got != 87 {
		t.Fatalf("Engine A/B parity matrix distinct primitive rows=%d, want 87", got)
	}
}

// TestEngineABParity_ComputeMatrix is the first tranche of the executable
// 87-row proof. Every row is a real Engine-A registry call paired with a real
// Blue runtime program: the result is observed after Engine B evaluates the
// opcode through bluehost, not by inspecting a manifest or a handler table.
//
// The complete all-87 driver below keeps one subtest per manifest row. This
// tranche is kept separately while the event/exec/leaf runners are added so
// a failure names the exact primitive and the exact observable boundary.
func TestEngineABParity_ComputeMatrix(t *testing.T) {
	for _, entry := range conformance.Manifest() {
		node, ok := conformance.Classify(entry.NodeID)
		if !ok || node.Kind != conformance.KindCompute {
			continue
		}
		t.Run(entry.NodeID, func(t *testing.T) {
			parity87RunCompute(t, entry.NodeID)
		})
	}
}

func parity87RunCompute(t *testing.T, id string) {
	t.Helper()
	signature, ok := conformance.Signatures()[id]
	if !ok {
		t.Fatalf("primitive=%s signature missing", id)
	}
	inputs, config := parity87ComputeCase(id, signature)
	registry := NewComputeRegistry()
	aFn, err := registry.Get(id)
	if err != nil {
		t.Fatalf("primitive=%s Engine A registry: %v", id, err)
	}
	aRaw, err := aFn(inputs, config)
	if err != nil {
		t.Fatalf("primitive=%s Engine A compute: %v", id, err)
	}
	if !json.Valid(aRaw) {
		t.Fatalf("primitive=%s Engine A returned invalid JSON: %s", id, aRaw)
	}
	if id == "core.output@1" {
		parity87RunOutput(t, inputs, config, aRaw)
		return
	}

	bValues := parity87RunBlueCompute(t, id, signature, inputs, config)
	aValue := parity87DecodeJSON(t, id, "Engine A", aRaw)
	if id == "core.source.read@1" {
		if got, want := canonicalJSONForTest(bValues), canonicalJSONForTest(aValue); !bytes.Equal(got, want) {
			t.Fatalf("primitive=%s source observable diverges: Engine A=%s Engine B=%s", id, want, got)
		}
		return
	}
	if len(signature.Outputs) > 1 {
		aOutputs, ok := aValue.(map[string]any)
		if ok {
			for _, output := range signature.Outputs {
				want, exists := aOutputs[output]
				if !exists {
					t.Fatalf("primitive=%s Engine A omitted declared output %q: %#v", id, output, aOutputs)
				}
				got, exists := bValues[output]
				if !exists {
					t.Fatalf("primitive=%s Engine B omitted declared output %q: %#v", id, output, bValues)
				}
				if gotJSON, wantJSON := canonicalJSONForTest(got), canonicalJSONForTest(want); !bytes.Equal(gotJSON, wantJSON) {
					t.Fatalf("primitive=%s output %q diverges: Engine A=%s Engine B=%s", id, output, wantJSON, gotJSON)
				}
			}
			return
		}
	}

	primary := parity87PrimaryOutput(id, signature.Outputs)
	bValue, ok := bValues[primary]
	if !ok {
		t.Fatalf("primitive=%s Engine B did not expose primary output %q: %#v", id, primary, bValues)
	}
	if got, want := canonicalJSONForTest(bValue), canonicalJSONForTest(aValue); !bytes.Equal(got, want) {
		t.Fatalf("primitive=%s primary output %q diverges: Engine A=%s Engine B=%s", id, primary, want, got)
	}
}

func parity87RunBlueCompute(t *testing.T, id string, signature conformance.NodeSignature, inputs, config map[string]json.RawMessage) map[string]any {
	t.Helper()
	program := parity87BuildPureProgram(t, id, signature, inputs, config)
	host := parityPrepareBHost(t, bluehost.SlotOnAir, "ab-87-"+parity87ProgramID(id), program)
	step := parityBStep(t, host, bluehost.SlotOnAir, id+" dedicated compute")
	values := make(map[string]any, len(signature.Outputs))
	for _, output := range signature.Outputs {
		name := parity87OutputVariable(output)
		if value, ok := step.Variables[name]; ok {
			values[output] = value
		}
	}
	if id == "core.source.read@1" {
		// Engine A's ComputeFn deliberately returns the complete resolved
		// source object; Engine B exposes the four declared output pins.
		// Reconstruct that same object at the host observation boundary.
		return map[string]any{
			"name":       values["name"],
			"kind":       values["kind"],
			"descriptor": values["descriptor"],
			"config":     values["config"],
		}
	}
	return values
}

func parity87BuildPureProgram(t *testing.T, id string, signature conformance.NodeSignature, inputs, config map[string]json.RawMessage) []byte {
	t.Helper()
	opcodes := []any{
		map[string]any{
			"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{},
			"inputs": []any{}, "outputs": []any{parityExecPort("then")},
		},
		map[string]any{
			"id": id, "kind": "pure",
			"config":  parity87ConfigPorts(id, signature.Config),
			"inputs":  parity87DataPorts(signature.Inputs),
			"outputs": parity87DataPorts(signature.Outputs),
		},
		map[string]any{
			"id": "core.variable.set@1", "kind": "pure",
			"config":  []any{parityDataPort("variable", "core.string", true)},
			"inputs":  []any{parityDataPort("value", "core.json", true), parityExecPort("in")},
			"outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")},
		},
	}
	nodes := []any{
		map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		map[string]any{"id": "target", "opcode": id, "config": parity87ConfigValues(id, config)},
	}
	execEdges := []any{}
	dataEdges := []any{}
	variables := []any{}
	for _, output := range signature.Outputs {
		mark := "mark-" + parity87ProgramID(output)
		variable := parity87OutputVariable(output)
		nodes = append(nodes, map[string]any{
			"id": mark, "opcode": "core.variable.set@1", "config": map[string]any{"variable": variable},
		})
		execEdges = append(execEdges,
			map[string]any{"from_node": "start-node", "from_port": "then", "to_node": mark, "to_port": "in", "sequence": len(execEdges)},
		)
		dataEdges = append(dataEdges, map[string]any{
			"from_node": "target", "from_port": output, "to_node": mark, "to_port": "value",
		})
		variables = append(variables, map[string]any{"name": variable, "type": "core.json", "initial": nil})
	}
	for _, input := range signature.Inputs {
		value, ok := inputs[input]
		if !ok {
			t.Fatalf("primitive=%s missing dedicated input fixture %q", id, input)
		}
		// Direct data literals are the canonical blue.program.v1 fixture
		// representation for a leaf input in this test. They still travel
		// through the runtime's input-resolution path; no handler is called
		// directly on Engine B.
		_ = value
	}
	inputNames := make([]string, 0, len(inputs))
	for input := range inputs {
		inputNames = append(inputNames, input)
	}
	sort.Strings(inputNames)
	dataLiterals := make([]any, 0, len(inputs))
	for _, input := range inputNames {
		value := inputs[input]
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			t.Fatalf("primitive=%s fixture input %q: %v", id, input, err)
		}
		dataLiterals = append(dataLiterals, map[string]any{"node_id": "target", "port": input, "value": decoded})
	}
	return parityBuildProgram(t, "ab-87-"+parity87ProgramID(id), opcodes, nodes,
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		execEdges, dataEdges, dataLiterals, []any{}, variables)
}

func parity87DataPorts(names []string) []any {
	ports := make([]any, 0, len(names))
	for _, name := range names {
		ports = append(ports, parityDataPort(name, "core.json", false))
	}
	return ports
}

func parity87ConfigPorts(id string, names []string) []any {
	allNames := append([]string(nil), names...)
	if id == "core.source.read@1" {
		allNames = append(allNames, "resolved_source")
	}
	sort.Strings(allNames)
	ports := make([]any, 0, len(allNames))
	for _, name := range allNames {
		ports = append(ports, parityDataPort(name, "core.json", false))
	}
	return ports
}

func parity87ComputeCase(id string, signature conformance.NodeSignature) (map[string]json.RawMessage, map[string]json.RawMessage) {
	inputs := make(map[string]json.RawMessage, len(signature.Inputs))
	for _, name := range signature.Inputs {
		switch name {
		case "a":
			if strings.HasPrefix(id, "core.logic.") {
				inputs[name] = raw(`true`)
			} else {
				inputs[name] = raw(`2`)
			}
		case "b":
			inputs[name] = raw(`3`)
		case "alpha":
			inputs[name] = raw(`0.25`)
		case "condition":
			inputs[name] = raw(`true`)
		case "when_true":
			inputs[name] = raw(`"selected"`)
		case "when_false":
			inputs[name] = raw(`"rejected"`)
		case "template":
			inputs[name] = raw(`"hello {name}"`)
		case "args":
			inputs[name] = raw(`{"name":"Ada"}`)
		case "value":
			switch {
			case strings.HasPrefix(id, "core.string."):
				if id == "core.string.split@1" {
					inputs[name] = raw(`"a,b,c"`)
				} else {
					inputs[name] = raw(`"Blue"`)
				}
			case strings.HasPrefix(id, "core.cast."):
				inputs[name] = raw(`"3.75"`)
			case id == "core.data.set-field@1":
				inputs[name] = raw(`true`)
			default:
				inputs[name] = raw(`-2.5`)
			}
		case "record":
			inputs[name] = raw(`{"user":{"name":"Ada"},"items":[1,2,3]}`)
		case "list":
			inputs[name] = raw(`[1,2,3]`)
		case "index":
			inputs[name] = raw(`1`)
		case "element":
			inputs[name] = raw(`"tail"`)
		case "items":
			inputs[name] = raw(`[1,2,3]`)
		case "plan":
			inputs[name] = raw(`{"table":"players","where":[],"joins":[],"select":[],"order":[]}`)
		case "n":
			inputs[name] = raw(`3`)
		default:
			inputs[name] = raw(`null`)
		}
	}

	config := map[string]json.RawMessage{}
	switch id {
	case "core.data.get-field@1":
		config["path"] = raw(`"user.name"`)
	case "core.data.set-field@1":
		config["path"] = raw(`"user.active"`)
	case "core.data.aggregate@1":
		config["op"] = raw(`"sum"`)
	case "core.db.from@1":
		config["table"] = raw(`"players"`)
	case "core.db.where@1":
		config["column"] = raw(`"name"`)
		config["op"] = raw(`"="`)
	case "core.db.join@1":
		config["table"] = raw(`"scores"`)
		config["local_column"] = raw(`"id"`)
		config["foreign_column"] = raw(`"player_id"`)
		config["select"] = raw(`["points"]`)
	case "core.db.select@1":
		config["columns"] = raw(`["id","name"]`)
	case "core.db.order@1":
		config["column"] = raw(`"name"`)
		config["direction"] = raw(`"desc"`)
	case "core.db.limit@1":
		config["n"] = raw(`5`)
	case "core.source.read@1":
		resolved := raw(`{"name":"players","kind":"query","descriptor":{"table":"players"},"config":{"datasource":"truth"}}`)
		config["source_id"] = raw(`"players-source"`)
		config[resolvedSourceConfigKey] = resolved
	}
	return inputs, config
}

// core.output is classified as KindCompute in Orion because Engine A's
// reactive registry exposes its passthrough value through the graph leaf,
// while Blue's portable opcode is an exec-triggered output sink. Keep this
// row on the same A/B harness, but observe each runtime at its actual public
// output boundary rather than pretending the `then` pin is data.
func parity87RunOutput(t *testing.T, inputs, config map[string]json.RawMessage, aRaw []byte) {
	t.Helper()
	opcodes := []any{
		map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
		map[string]any{"id": "core.output@1", "kind": "control", "config": []any{parityDataPort("name", "core.string", false)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityExecPort("then")}},
	}
	nodes := []any{
		map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		map[string]any{"id": "output", "opcode": "core.output@1", "config": map[string]any{"name": "result"}},
	}
	execEdges := []any{map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "output", "to_port": "in", "sequence": 0}}
	dataLiterals := []any{}
	for name, value := range inputs {
		if name == "in" {
			// `in` is the exec trigger for the portable output sink,
			// not a data literal. The Engine-A compute ABI lists it in
			// the signature because the graph compiler carries the
			// trigger alongside the value input.
			continue
		}
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			t.Fatalf("primitive=core.output@1 Engine B input %q: %v", name, err)
		}
		dataLiterals = append(dataLiterals, map[string]any{"node_id": "output", "port": name, "value": decoded})
	}
	program := parityBuildProgram(t, "ab-87-core-output", opcodes, nodes,
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		execEdges, []any{}, dataLiterals, []any{}, []any{})
	host := parityPrepareBHost(t, bluehost.SlotOnAir, "ab-87-core-output", program)
	step := parityBStep(t, host, bluehost.SlotOnAir, "core.output@1 dedicated compute")
	outputs, ok := step.Variables["__outputs__"].(map[string]any)
	if !ok {
		t.Fatalf("primitive=core.output@1 Engine B reserved outputs missing: %#v", step.Variables)
	}
	aValue := parity87DecodeJSON(t, "core.output@1", "Engine A", aRaw)
	bValue, ok := outputs["result"]
	if !ok {
		t.Fatalf("primitive=core.output@1 Engine B output name missing: %#v", outputs)
	}
	if got, want := canonicalJSONForTest(bValue), canonicalJSONForTest(aValue); !bytes.Equal(got, want) {
		t.Fatalf("primitive=core.output@1 output diverges: Engine A=%s Engine B=%s", want, got)
	}
	_ = config
}

func parity87ConfigValues(id string, config map[string]json.RawMessage) map[string]any {
	values := make(map[string]any, len(config)+1)
	for key, value := range config {
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			panic(fmt.Sprintf("parity87 config %s/%s: %v", id, key, err))
		}
		if id == "core.source.read@1" && key == resolvedSourceConfigKey {
			values["resolved_source"] = decoded
			continue
		}
		values[key] = decoded
	}
	return values
}

func parity87OutputVariable(output string) string {
	return "out_" + parity87ProgramID(output)
}

func parity87PrimaryOutput(id string, outputs []string) string {
	for _, preferred := range []string{"result", "value", "plan", "sum", "diff", "product", "quotient", "remainder", "count", "parts", "element"} {
		for _, output := range outputs {
			if output == preferred {
				return output
			}
		}
	}
	if len(outputs) == 0 {
		panic(fmt.Sprintf("primitive=%s has no declared output", id))
	}
	return outputs[0]
}

func parity87DecodeJSON(t *testing.T, id, engine string, value []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("primitive=%s %s JSON decode: %v", id, engine, err)
	}
	return decoded
}

func parity87ProgramID(value string) string {
	value = strings.TrimSuffix(value, "@1")
	value = strings.NewReplacer(".", "-", "/", "-", "_", "-").Replace(value)
	return value
}

func parity87RunEntry(t *testing.T, id string) {
	t.Helper()
	switch id {
	case "core.event.on-start@1":
		parity87RunStartEntry(t)
	case "core.event.on-tick@1":
		parity87RunHostEntry(t, "tick", "core.event.on-tick@1")
	case "core.operator.on-call@1":
		parity87RunHostEntry(t, "call", "core.operator.on-call@1")
	case "core.event.on-event@1":
		// This row carries the FIFO proof as its dedicated event
		// observable: two accepted events must produce the same ordered
		// trace in both runtimes.
		TestEngineABParity_EventFIFOOrderingThroughHost(t)
	case "core.event.on-platform-event@1":
		// The canonical active ingress path is the observable boundary
		// shared with Quasar. Its companion suite also covers duplicate,
		// gap, conflict, origin and preview isolation semantics.
		TestEngineABParity_PlatformIngressUsesCanonicalActiveAdapter(t)
	default:
		t.Fatalf("primitive=%s has no dedicated entrypoint runner", id)
	}
}

func parity87RunStartEntry(t *testing.T) {
	a := execScene(t, "ab-87-on-start-a", &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"mark": constSet("mark", "result", `"started"`),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "mark"}},
		},
	})
	startScene(t, a)
	mustFire(t, a, "start")
	waitForState(t, a, "__vars.bp.result", `"started"`, 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.result")

	b := parityPrepareBHost(t, bluehost.SlotOnAir, "ab-87-on-start-b", parity87BuildBStartProgram(t))
	bStep := parityBStep(t, b, bluehost.SlotOnAir, "core.event.on-start@1 dedicated entry")
	parity87AssertJSONEqual(t, "core.event.on-start@1", aValue, bStep.Variables["result"])
}

func parity87BuildBStartProgram(t *testing.T) []byte {
	t.Helper()
	return parityBuildProgram(t, "ab-87-on-start",
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		[]any{map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0}},
		[]any{},
		[]any{map[string]any{"node_id": "mark", "port": "value", "value": "started"}},
		[]any{},
		[]any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
	)
}

func parity87RunHostEntry(t *testing.T, kind, primitive string) {
	t.Helper()
	leaf := "__inputs.platform.twitch.channel_1.last_chat"
	payload := `{"type":"chat","payload":{"text":"hello"}}`
	a := execScene(t, "ab-87-"+kind+"-a", parityEngineAEntryProgram(kind, leaf))
	startScene(t, a)

	var aValue []byte
	switch kind {
	case "tick":
		a.Input(InputMsg{Path: tickPath, Value: raw(`1000`), Source: "system:tick", IsSystem: true})
		a.Input(InputMsg{Path: tickPath, Value: raw(`2500`), Source: "system:tick", IsSystem: true})
		waitForState(t, a, "__vars.bp.result", `1.5`, 2*time.Second)
	case "call":
		if !a.FireOnCall("bp/trigger", raw(payload)) {
			t.Fatalf("primitive=%s Engine A rejected on-call trigger", primitive)
		}
		waitForState(t, a, "__vars.bp.result", payload, 2*time.Second)
	default:
		t.Fatalf("primitive=%s unsupported host entry kind %q", primitive, kind)
	}
	aValue, _ = a.state.Get("__vars.bp.result")

	h := parityPrepareBHost(t, bluehost.SlotOnAir, "ab-87-"+kind+"-b", parityBuildBEntrypointProgram(t, kind, leaf))
	var bStep blueruntime.StepResult
	var err error
	switch kind {
	case "tick":
		bStep, err = h.Tick(bluehost.SlotOnAir, 1.5)
	case "call":
		bStep, err = h.Call(bluehost.SlotOnAir, "trigger", map[string]any{"type": "chat", "payload": map[string]any{"text": "hello"}})
	}
	if err != nil {
		t.Fatalf("primitive=%s Engine B trigger: %v", primitive, err)
	}
	parity87AssertJSONEqual(t, primitive, aValue, bStep.Outputs["result"])
}

func parity87RunLeaf(t *testing.T, id string) {
	t.Helper()
	const sourceValue = `7`
	var graph *compiler.Graph
	switch id {
	case "core.input@1":
		graph = parity87LeafGraph(id, "input.value", raw(`null`), "input.value")
	case "core.literal@1":
		graph = parity87LeafGraph(id, "literal.value", raw(sourceValue), "literal.value")
	case "core.variable.get@1":
		graph = parity87LeafGraph(id, "__vars.bp.source", raw(sourceValue), "__vars.bp.source")
	default:
		t.Fatalf("primitive=%s has no leaf-bound runner", id)
	}
	a := NewScene("ab-87-"+parity87ProgramID(id)+"-a", graph, &compiler.RenderBundle{SceneVersion: "sha256:ab-87"}, NewComputeRegistry(), quietLogger())
	aValue, ok := a.state.Get("result")
	if !ok {
		t.Fatalf("primitive=%s Engine A result leaf missing", id)
	}

	b := parityPrepareBHost(t, bluehost.SlotOnAir, "ab-87-"+parity87ProgramID(id)+"-b", parity87BuildBLeafProgram(t, id))
	bStep := parityBStep(t, b, bluehost.SlotOnAir, id+" dedicated leaf")
	parity87AssertJSONEqual(t, id, aValue, bStep.Variables["result"])
}

func parity87LeafGraph(id, inputPath string, initial json.RawMessage, nodePath string) *compiler.Graph {
	return &compiler.Graph{
		SceneID:      "ab-87-" + parity87ProgramID(id),
		SceneVersion: "sha256:ab-87-leaf",
		Nodes: []compiler.GraphNode{
			{ID: "source", Kind: "input", Path: nodePath, Compute: id},
			{ID: "result", Kind: "output", Path: "result", Compute: "core.output@1", Upstream: []string{"source"}, Inputs: []compiler.GraphInput{{From: "source", Port: "value"}}},
		},
		Defaults: map[string]json.RawMessage{inputPath: initial},
	}
}

func parity87BuildBLeafProgram(t *testing.T, id string) []byte {
	t.Helper()
	opcodeID := id
	config := map[string]any{}
	variables := []any{map[string]any{"name": "result", "type": "core.json", "initial": nil}}
	switch id {
	case "core.input@1":
		config["name"] = "input.value"
	case "core.literal@1":
		config["value"] = json.Number("7")
	case "core.variable.get@1":
		config["variable"] = "source"
		variables = append(variables, map[string]any{"name": "source", "type": "core.json", "initial": json.Number("7")})
	default:
		t.Fatalf("primitive=%s has no Blue leaf fixture", id)
	}
	return parityBuildProgram(t, "ab-87-"+parity87ProgramID(id),
		[]any{
			map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
			map[string]any{"id": opcodeID, "kind": "pure", "config": parity87LeafConfigPorts(id), "inputs": []any{}, "outputs": []any{parityDataPort("value", "core.json", false)}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "leaf", "opcode": opcodeID, "config": config},
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
			map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		},
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		[]any{map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0}},
		[]any{map[string]any{"from_node": "leaf", "from_port": "value", "to_node": "mark", "to_port": "value"}},
		[]any{}, []any{}, variables,
	)
}

func parity87LeafConfigPorts(id string) []any {
	name := "name"
	if id == "core.literal@1" {
		name = "value"
	} else if id == "core.variable.get@1" {
		name = "variable"
	}
	return []any{parityDataPort(name, "core.json", false)}
}

func parity87RunPlatformFamily(t *testing.T, id string) {
	t.Helper()
	name := strings.TrimSuffix(strings.TrimPrefix(id, "quasar.twitch."), "@1")
	leaf := "__inputs.platform.twitch.channel_1.last_" + name
	payload := map[string]any{"event": id, "value": 42}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("primitive=%s marshal platform payload: %v", id, err)
	}

	graph := &compiler.Graph{
		SceneID:      "ab-87-" + parity87ProgramID(id),
		SceneVersion: "sha256:ab-87-platform",
		Nodes: []compiler.GraphNode{
			{ID: "family", Kind: "input", Path: leaf, Compute: id},
			{ID: "result", Kind: "output", Path: "result", Compute: "core.output@1", Upstream: []string{"family"}, Inputs: []compiler.GraphInput{{From: "family", Port: "value"}}},
		},
		Defaults: map[string]json.RawMessage{leaf: raw(`null`)},
		Bindings: []compiler.ExternalAdapter{{Key: leaf, Label: "Quasar platform stream", Kind: "platform-stream", TargetPaths: []string{leaf}}},
	}
	a := NewScene("ab-87-"+parity87ProgramID(id)+"-a", graph, &compiler.RenderBundle{SceneVersion: "sha256:ab-87-platform"}, NewComputeRegistry(), quietLogger())
	startScene(t, a)
	if !a.Input(InputMsg{Path: leaf, Value: payloadRaw, Source: "platform:twitch"}) {
		t.Fatalf("primitive=%s Engine A rejected canonical platform leaf %q", id, leaf)
	}
	waitForState(t, a, "result", string(payloadRaw), 2*time.Second)
	aValue, _ := a.state.Get("result")

	b := parityPrepareBHost(t, bluehost.SlotOnAir, "ab-87-"+parity87ProgramID(id)+"-b", parity87BuildBPlatformFamilyProgram(t, id, leaf))
	bStep, err := b.WritePlatformEvent(bluehost.SlotOnAir, leaf, payload)
	if err != nil {
		t.Fatalf("primitive=%s Engine B canonical platform write: %v", id, err)
	}
	parity87AssertJSONEqual(t, id, aValue, bStep.Outputs["result"])
}

func parity87BuildBPlatformFamilyProgram(t *testing.T, id, leaf string) []byte {
	t.Helper()
	return parityBuildProgram(t, "ab-87-"+parity87ProgramID(id),
		[]any{
			map[string]any{"id": "core.event.on-platform-event@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityDataPort("payload", "core.json", false), parityExecPort("then")}},
			map[string]any{"id": id, "kind": "pure", "config": []any{parityDataPort("channel", "core.string", false)}, "inputs": []any{}, "outputs": []any{parityDataPort("channel", "core.string", false), parityDataPort("payload", "core.json", false)}},
			map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}},
		},
		[]any{
			map[string]any{"id": "event-node", "opcode": "core.event.on-platform-event@1", "config": map[string]any{}},
			map[string]any{"id": "family", "opcode": id, "config": map[string]any{"channel": "channel_1"}},
			map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "result"}},
		},
		[]any{map[string]any{"id": "platform", "kind": "platform-event", "node_id": "event-node", "port": "then", "leaf": leaf}},
		[]any{map[string]any{"from_node": "event-node", "from_port": "then", "to_node": "mark", "to_port": "in", "sequence": 0}},
		[]any{map[string]any{"from_node": "family", "from_port": "payload", "to_node": "mark", "to_port": "value"}},
		[]any{}, []any{}, []any{map[string]any{"name": "result", "type": "core.json", "initial": nil}},
	)
}

func parity87RunExecOp(t *testing.T, id, op string) {
	t.Helper()
	switch id {
	case "core.http.request@1":
		testParityInventoryHTTP(t)
	case "core.http-request@1":
		parity87RunHTTPAlias(t)
	case "core.db.query@1":
		TestEngineABParity_DBQueryObservableAndPreviewNoQuery(t)
	case "core.service.call@1":
		testParityInventoryService(t)
	case "core.show.emit@1":
		testParityInventoryShow(t)
	case "core.overlay-app.set@1":
		testParityInventoryOverlay(t)
	case "core.animation.play@1":
		testParityInventoryAnimation(t)
	case "core.operator.await-value@1":
		testParityInventoryAwait(t)
	case "core.flow.gate@1":
		testParityInventoryGate(t)
	case "core.flow.delay@1":
		testParityInventoryDelay(t)
	default:
		parity87RunSimpleExec(t, id, op)
	}
}

func parity87RunSimpleExec(t *testing.T, id, op string) {
	t.Helper()
	if op == "" {
		t.Fatalf("primitive=%s has no exec operation mapping", id)
	}
	a := execScene(t, "ab-87-"+parity87ProgramID(id)+"-a", execOpProbeProgram(op))
	if isWorldOp(op) {
		a.SetValidationMode()
	}
	startScene(t, a)
	mustFire(t, a, "e")
	if op == OpOperatorAwait {
		waitForPendingAwait(t, a, "bp", "conf", 2*time.Second)
		if err := a.ResolveAwait("bp", "conf", raw(`"v"`)); err != nil {
			t.Fatalf("primitive=%s Engine A await resolve: %v", id, err)
		}
	}
	waitForState(t, a, "__vars.bp.reached", `true`, 2*time.Second)

	b := parityPrepareBHost(t, bluehost.SlotOnAir, "ab-87-"+parity87ProgramID(id)+"-b", parity87BuildBSimpleExecProgram(t, id, op))
	bStep := parityBStep(t, b, bluehost.SlotOnAir, id+" dedicated exec")
	if reached, ok := bStep.Variables["reached"].(bool); !ok || !reached {
		t.Fatalf("primitive=%s Engine B continuation did not reach marker: %#v", id, bStep.Variables)
	}
}

func parity87BuildBSimpleExecProgram(t *testing.T, id, op string) []byte {
	t.Helper()
	targetInput := "in"
	continuation := "then"
	var inputs []any
	var outputs []any
	config := map[string]any{}
	literals := []any{}
	kind := "control"
	switch op {
	case OpBranch:
		inputs = []any{parityDataPort("condition", "core.json", true), parityExecPort("in")}
		outputs = []any{parityExecPort("false"), parityExecPort("true")}
		literals = append(literals, map[string]any{"node_id": "target", "port": "condition", "value": true})
		continuation = "true"
	case OpSequence:
		inputs = []any{parityExecPort("in")}
		outputs = []any{parityExecPort("then_0"), parityExecPort("then_1"), parityExecPort("then_2")}
		continuation = "then_0"
	case OpForLoop:
		inputs = []any{parityDataPort("first", "core.json", true), parityDataPort("last", "core.json", true), parityExecPort("in")}
		outputs = []any{parityDataPort("index", "core.json", false), parityExecPort("body"), parityExecPort("completed")}
		literals = append(literals,
			map[string]any{"node_id": "target", "port": "first", "value": 0},
			map[string]any{"node_id": "target", "port": "last", "value": 0})
		continuation = "completed"
	case OpForEach:
		inputs = []any{parityDataPort("items", "core.json", true), parityExecPort("in")}
		outputs = []any{parityDataPort("element", "core.json", false), parityDataPort("index", "core.json", false), parityExecPort("body"), parityExecPort("completed")}
		literals = append(literals, map[string]any{"node_id": "target", "port": "items", "value": []any{1}})
		continuation = "completed"
	case OpWhile:
		inputs = []any{parityDataPort("condition", "core.json", true), parityExecPort("in")}
		outputs = []any{parityExecPort("body"), parityExecPort("completed")}
		literals = append(literals, map[string]any{"node_id": "target", "port": "condition", "value": false})
		continuation = "completed"
	case OpVariableSet:
		kind = "pure"
		inputs = []any{parityDataPort("value", "core.json", true), parityExecPort("in")}
		outputs = []any{parityDataPort("value", "core.json", false), parityExecPort("then")}
		config["variable"] = "scratch"
		literals = append(literals, map[string]any{"node_id": "target", "port": "value", "value": true})
	case OpPrint:
		kind = "pure"
		inputs = []any{parityDataPort("value", "core.json", true), parityExecPort("in")}
		outputs = []any{parityExecPort("then")}
		literals = append(literals, map[string]any{"node_id": "target", "port": "value", "value": "conf"})
	default:
		t.Fatalf("primitive=%s unsupported simple exec op %q", id, op)
	}
	opcodes := []any{
		map[string]any{"id": "core.event.on-start@1", "kind": "entrypoint", "config": []any{}, "inputs": []any{}, "outputs": []any{parityExecPort("then")}},
		map[string]any{"id": id, "kind": kind, "config": parity87SimpleExecConfigPorts(op), "inputs": inputs, "outputs": outputs},
	}
	if id != "core.variable.set@1" {
		opcodes = append(opcodes, map[string]any{"id": "core.variable.set@1", "kind": "pure", "config": []any{parityDataPort("variable", "core.string", true)}, "inputs": []any{parityDataPort("value", "core.json", true), parityExecPort("in")}, "outputs": []any{parityDataPort("value", "core.json", false), parityExecPort("then")}})
	}
	nodes := []any{
		map[string]any{"id": "mark", "opcode": "core.variable.set@1", "config": map[string]any{"variable": "reached"}},
		map[string]any{"id": "start-node", "opcode": "core.event.on-start@1", "config": map[string]any{}},
		map[string]any{"id": "target", "opcode": id, "config": config},
	}
	execEdges := []any{
		map[string]any{"from_node": "start-node", "from_port": "then", "to_node": "target", "to_port": targetInput, "sequence": 0},
		map[string]any{"from_node": "target", "from_port": continuation, "to_node": "mark", "to_port": "in", "sequence": 0},
	}
	literals = append(literals, map[string]any{"node_id": "mark", "port": "value", "value": true})
	sort.SliceStable(literals, func(i, j int) bool {
		a := literals[i].(map[string]any)
		b := literals[j].(map[string]any)
		if a["node_id"] != b["node_id"] {
			return a["node_id"].(string) < b["node_id"].(string)
		}
		return a["port"].(string) < b["port"].(string)
	})
	return parityBuildProgram(t, "ab-87-"+parity87ProgramID(id), opcodes, nodes,
		[]any{map[string]any{"id": "start", "kind": "start", "node_id": "start-node", "port": "then"}},
		execEdges, []any{}, literals, []any{}, []any{map[string]any{"name": "reached", "type": "core.json", "initial": nil}})
}

func parity87SimpleExecConfigPorts(op string) []any {
	if op == OpVariableSet {
		return []any{parityDataPort("variable", "core.string", false)}
	}
	return []any{}
}

func parity87RunHTTPAlias(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	egress := loopbackEgress(t, srv.URL)

	a := effectsScene(t, "ab-87-http-alias-a", &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"request": {ID: "request", Op: OpHTTPRequest, Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `"`)}, Next: map[string]ExecTarget{"then": {Node: "status"}}},
			"status":  setFromPin("status", "status", "request", "status", nil),
		},
		Entrypoints: map[string]ExecEntry{"start": {Target: ExecTarget{Node: "request"}}},
	}, &SceneEffects{Runner: newTestRunner(t), Egress: egress})
	startScene(t, a)
	mustFire(t, a, "start")
	waitForState(t, a, "__vars.bp.status", "201", 2*time.Second)
	aValue, _ := a.state.Get("__vars.bp.status")

	program := buildEngineBHTTPProgram(t, srv.URL)
	var doc map[string]any
	if err := json.Unmarshal(program, &doc); err != nil {
		t.Fatalf("primitive=core.http-request@1 parse canonical fixture: %v", err)
	}
	for _, rawOpcode := range doc["opcodes"].([]any) {
		if opcode, ok := rawOpcode.(map[string]any); ok && opcode["id"] == "core.http.request@1" {
			opcode["id"] = "core.http-request@1"
		}
	}
	for _, rawNode := range doc["nodes"].([]any) {
		if node, ok := rawNode.(map[string]any); ok && node["opcode"] == "core.http.request@1" {
			node["opcode"] = "core.http-request@1"
		}
	}
	delete(doc, "program_digest")
	doc["program_digest"] = digestOfForTest(doc)
	alias, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("primitive=core.http-request@1 marshal alias fixture: %v", err)
	}
	h := bluehost.NewHost()
	t.Cleanup(func() { _ = h.Release(bluehost.SlotOnAir, "test-cleanup") })
	if err := h.Prepare(bluehost.SlotOnAir, "ab-87-http-alias-b", "scene-ab-87-http-alias-b", "sha256:ab-87-http-alias-b", alias, nil, nil,
		bluehost.NewEffectHandlers(bluehost.EffectDeps{Egress: egress}, blueruntime.Execute)); err != nil {
		t.Fatalf("primitive=core.http-request@1 Engine B Prepare: %v", err)
	}
	bStep := parityBStep(t, h, bluehost.SlotOnAir, "core.http-request@1 dedicated alias")
	parity87AssertJSONEqual(t, "core.http-request@1", aValue, bStep.Outputs["result"])
}

func parity87AssertJSONEqual(t *testing.T, id string, aValue []byte, bValue any) {
	t.Helper()
	a := parity87DecodeJSON(t, id, "Engine A", aValue)
	if got, want := canonicalJSONForTest(bValue), canonicalJSONForTest(a); !bytes.Equal(got, want) {
		t.Fatalf("primitive=%s observable diverges: Engine A=%s Engine B=%s", id, want, got)
	}
}
