package compiler

import (
	"encoding/json"
	"errors"
	"testing"
)

// Unit tests for the in-body exec-compile seam (ADR 015 Amendment 1 §A1.3).
// These prove the seam compiles the exec layer from an authoring graph with
// NO Fetcher (zero egress), folds variables[].value into the __vars.. seeds
// (Orion #192 mechanism), and rejects a `reference` node (REFERENCE_NOT_
// SUPPORTED) — the invariant that keeps the no-egress clearance valid.

func execInP(n string) BlueprintPort  { return BlueprintPort{Name: n, Type: "exec", Kind: "exec"} }
func execOutP(n string) BlueprintPort { return BlueprintPort{Name: n, Type: "exec", Kind: "exec"} }
func dataP(n string) BlueprintPort    { return BlueprintPort{Name: n, Type: "any", Kind: "data"} }

// TestCompileExecPrograms_OnStartProgram: a minimal authoring graph compiles
// to exactly one exec program with an on-start entry targeting the set node.
func TestCompileExecPrograms_OnStartProgram(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1", Outputs: []BlueprintPort{execOutP("then")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"x"`)},
				Inputs:  []BlueprintPort{execInP("exec_in")},
				Outputs: []BlueprintPort{execOutP("then")}},
		},
		Edges: []BlueprintEdge{{FromNode: "start", FromPort: "then", ToNode: "set", ToPort: "exec_in"}},
	}
	out, cerr := CompileExecPrograms(bp, "")
	if cerr != nil {
		t.Fatalf("unexpected compile error: %v", cerr)
	}
	if len(out.Programs) != 1 {
		t.Fatalf("want 1 program, got %d", len(out.Programs))
	}
	// The emitted bytes carry the on-start entry targeting "set".
	var prog struct {
		Entrypoints map[string]struct {
			Kind   string `json:"kind"`
			Target struct {
				Node string `json:"node"`
			} `json:"target"`
		} `json:"entrypoints"`
	}
	if err := json.Unmarshal(out.Programs[0], &prog); err != nil {
		t.Fatalf("emitted program not valid JSON: %v", err)
	}
	e, ok := prog.Entrypoints["start"]
	if !ok || e.Kind != "on-start" || e.Target.Node != "set" {
		t.Fatalf("entry wrong: %+v", prog.Entrypoints)
	}
}

// TestCompileExecPrograms_VariablesSeedDefaults: a graph-level variable with a
// value is harvested into the __vars seed the inlined variable.get reads.
func TestCompileExecPrograms_VariablesSeedDefaults(t *testing.T) {
	bp := &BlueprintGraph{
		ID:    "bp",
		Nodes: []BlueprintNode{{ID: "start", Compute: "core.event.on-start@1", Outputs: []BlueprintPort{execOutP("then")}}},
		Variables: []BlueprintVariable{
			{ID: "v1", Name: "palette", Type: "list", Value: json.RawMessage(`["#fff","#000"]`)},
			{ID: "v2", Name: "shared", Type: "string"}, // no value → no seed
		},
	}
	out, cerr := CompileExecPrograms(bp, "")
	if cerr != nil {
		t.Fatalf("unexpected compile error: %v", cerr)
	}
	// Empty-key form: __vars..palette (prefixDefaultLeaf with key "").
	got, ok := out.Defaults["__vars..palette"]
	if !ok {
		t.Fatalf("palette variable not seeded into defaults: %+v", out.Defaults)
	}
	if string(got) != `["#fff","#000"]` {
		t.Fatalf("palette seed = %s", got)
	}
	if _, ok := out.Defaults["__vars..shared"]; ok {
		t.Fatalf("value-less variable must NOT be seeded: %+v", out.Defaults)
	}
}

// TestCompileExecPrograms_VariablesKeyNamespaced: under a non-empty blueprint
// key the variable seed is namespaced inside the __vars prefix, byte-identical
// to the leaf prefixGraphNodes writes on the reading variable.get.
func TestCompileExecPrograms_VariablesKeyNamespaced(t *testing.T) {
	bp := &BlueprintGraph{
		ID:    "bp",
		Nodes: []BlueprintNode{{ID: "start", Compute: "core.event.on-start@1", Outputs: []BlueprintPort{execOutP("then")}}},
		Variables: []BlueprintVariable{
			{ID: "v1", Name: "palette", Type: "list", Value: json.RawMessage(`[1]`)},
		},
	}
	out, cerr := CompileExecPrograms(bp, "scoreboard")
	if cerr != nil {
		t.Fatalf("unexpected compile error: %v", cerr)
	}
	if _, ok := out.Defaults["__vars.scoreboard.palette"]; !ok {
		t.Fatalf("expected key-namespaced seed __vars.scoreboard.palette, got %+v", out.Defaults)
	}
}

// TestCompileExecPrograms_RejectsReference: a reference node fails loud as
// REFERENCE_NOT_SUPPORTED (never a silent no-program), preserving no-egress.
func TestCompileExecPrograms_RejectsReference(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1", Outputs: []BlueprintPort{execOutP("then")}},
			{ID: "callee", Reference: &BlueprintReference{BlueprintID: "x", Version: 1}},
		},
	}
	_, cerr := CompileExecPrograms(bp, "")
	if cerr == nil {
		t.Fatalf("expected a compile error for a reference node")
	}
	if !cerr.HasCode(ErrReferenceNotSupported) {
		t.Fatalf("want REFERENCE_NOT_SUPPORTED, got %v", cerr)
	}
	if !errors.Is(cerr, ErrCompileFailed) {
		t.Fatalf("compile error should match ErrCompileFailed sentinel")
	}
}

// TestCompileExecPrograms_DanglingTarget: an entrypoint targeting an absent
// node is EXEC_UNKNOWN_NODE — the partition's validateExecTargets fires.
func TestCompileExecPrograms_DanglingTarget(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1", Outputs: []BlueprintPort{execOutP("then")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"x"`)},
				Inputs:  []BlueprintPort{execInP("exec_in")},
				Outputs: []BlueprintPort{execOutP("then")}},
		},
		Edges: []BlueprintEdge{{FromNode: "start", FromPort: "then", ToNode: "ghost", ToPort: "exec_in"}},
	}
	_, cerr := CompileExecPrograms(bp, "")
	if cerr == nil || !cerr.HasCode(ErrExecUnknownNode) {
		t.Fatalf("want EXEC_UNKNOWN_NODE, got %v", cerr)
	}
}

// TestCompileExecPrograms_PureDataflowNoProgram: a graph with no exec spine
// (no on-start/on-tick/on-event) produces zero exec programs. On the SIMULATE
// path that is an author error — a dry-run fires entrypoints against a
// synthetic event, and there is nothing to fire. The seam rejects it loud as
// NO_EXEC_PROGRAM rather than returning a silent `blueprints: null` (ADR 015
// §A1.3 step 4 / R7: jamais 200 muet). NB: the PUSH path still treats a
// pure-dataflow scene as trivially validated (Harness.Validate doc) — that
// path never calls CompileExecPrograms.
func TestCompileExecPrograms_PureDataflowNoProgram(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp",
		Nodes: []BlueprintNode{
			{ID: "in", Compute: "core.input@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"score"`)},
				Outputs: []BlueprintPort{dataP("out")}},
			{ID: "out", Compute: "core.output@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"display"`)},
				Inputs: []BlueprintPort{dataP("in")}},
		},
		Edges: []BlueprintEdge{{FromNode: "in", FromPort: "out", ToNode: "out", ToPort: "in"}},
	}
	_, cerr := CompileExecPrograms(bp, "")
	if cerr == nil || !cerr.HasCode(ErrNoExecProgram) {
		t.Fatalf("want NO_EXEC_PROGRAM for a graph with no exec spine, got %v", cerr)
	}
	if !errors.Is(cerr, ErrCompileFailed) {
		t.Fatalf("compile error should match ErrCompileFailed sentinel")
	}
}

// TestCompileExecPrograms_RejectsUnknownUnwiredNode: an unknown `definition`
// that is NOT wired into an exec spine (no exec pin) must still fail loud as
// UNKNOWN_NODE. This is the live-observed residue: `core.nonexistent.fake-
// node@99` left unwired slipped through the exec partition (it only fires
// EXEC_OP_UNMAPPED for an exec-pinned node) and the request returned a silent
// `blueprints: null`. The up-front conformance.Classify pass catches it
// regardless of pins. An on-start is included so the failure is the unknown
// node, NOT NO_EXEC_PROGRAM.
func TestCompileExecPrograms_RejectsUnknownUnwiredNode(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1", Outputs: []BlueprintPort{execOutP("then")}},
			// Unknown definition, no exec pin, no edge — a "data node" the
			// partition never routes to the exec layer.
			{ID: "ghost", Compute: "core.nonexistent.fake-node@99",
				Outputs: []BlueprintPort{dataP("out")}},
		},
	}
	_, cerr := CompileExecPrograms(bp, "")
	if cerr == nil || !cerr.HasCode(ErrUnknownNode) {
		t.Fatalf("want UNKNOWN_NODE for an unknown unwired node, got %v", cerr)
	}
	if !errors.Is(cerr, ErrCompileFailed) {
		t.Fatalf("compile error should match ErrCompileFailed sentinel")
	}
}
