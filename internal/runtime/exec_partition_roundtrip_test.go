package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// TestExecPartition_RoundTripFromCompiler is ADR 006 §6 criterion #1's
// round-trip clause: what the compiler partition (issue #103) EMITS into
// graph.ExecPrograms must re-read correctly through the interpreter's own
// deserializer (ExecProgramsFromGraph) into runtime.ExecProgram. The
// compiler mirrors the exec wire shape with its own structs (it cannot
// import the runtime — that would cycle); this test is the byte-pin that
// the two shapes agree. It drives the REAL compiler, so the artefact is
// exactly what a push produces.
func TestExecPartition_RoundTripFromCompiler(t *testing.T) {
	// on-start → branch(condition from literal) → variable.set(value
	// from literal). Pure-but-exec (branch) + effect-exec (variable.set)
	// + pure data (the two literals feeding exec data pins).
	execIn := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
	}
	execOut := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
	}
	dataIn := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "any", Kind: "data"}
	}

	bp := &compiler.BlueprintGraph{
		ID: "bp-exec",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{execOut("then")}},
			{ID: "cond", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`true`)},
				Outputs: []compiler.BlueprintPort{dataIn("out")}},
			{ID: "br", Compute: "core.flow.branch@1",
				Inputs:  []compiler.BlueprintPort{execIn("exec_in"), dataIn("condition")},
				Outputs: []compiler.BlueprintPort{execOut("true"), execOut("false")}},
			{ID: "v", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`9`)},
				Outputs: []compiler.BlueprintPort{dataIn("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"counter"`)},
				Inputs:  []compiler.BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []compiler.BlueprintPort{execOut("then")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "br", ToPort: "exec_in"},
			{FromNode: "cond", FromPort: "out", ToNode: "br", ToPort: "condition"},
			{FromNode: "br", FromPort: "true", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "v", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}
	f := &stubFetcher{
		layout: &compiler.CanvasLayout{
			Version: "v1",
			Root:    compiler.LayoutNode{Kind: "stack", ID: "root"},
		},
		blueprint: bp,
		manifest: compiler.ComputeManifest{
			"core.event.on-start@1": {IsPure: true, IsBounded: true, Version: "1"},
			"core.literal@1":        {IsPure: true, IsBounded: true, Version: "1"},
			"core.flow.branch@1":    {IsPure: true, IsBounded: true, Version: "1"},
			"core.variable.set@1":   {IsPure: true, IsBounded: true, Version: "1"},
		},
	}
	graph, _, _, err := compiler.Compile(context.Background(), "scene-exec",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-exec"}, f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// The whole point: the compiler-emitted raw bytes decode into the
	// runtime's OWN ExecProgram type without loss.
	progs, err := ExecProgramsFromGraph(graph)
	if err != nil {
		t.Fatalf("ExecProgramsFromGraph rejected the compiler's emission: %v", err)
	}
	if len(progs) != 1 {
		t.Fatalf("want 1 program, got %d", len(progs))
	}
	p := progs[0]

	// Entry decodes to an on-start ExecEntry targeting the branch.
	e, ok := p.Entrypoints["start"]
	if !ok || e.Kind != EntryOnStart || e.Target.Node != "br" {
		t.Fatalf("entry round-trip wrong: %+v", p.Entrypoints)
	}
	// Branch node decodes with the runtime op constant + Next + data.
	br, ok := p.Nodes["br"]
	if !ok || br.Op != OpBranch {
		t.Fatalf("branch op round-trip = %+v, want %q", br, OpBranch)
	}
	if tgt, ok := br.Next["true"]; !ok || tgt.Node != "set" {
		t.Fatalf("branch Next[true] round-trip = %+v", br.Next)
	}
	if len(br.Data) != 1 || br.Data[0].Port != "condition" || br.Data[0].From != "cond" {
		t.Fatalf("branch data round-trip = %+v", br.Data)
	}
	// variable.set decodes with op + verbatim config + data pull.
	set, ok := p.Nodes["set"]
	if !ok || set.Op != OpVariableSet {
		t.Fatalf("set op round-trip = %+v, want %q", set, OpVariableSet)
	}
	if string(set.Config["variable"]) != `"counter"` {
		t.Fatalf("set config.variable round-trip = %s", set.Config["variable"])
	}
	if len(set.Data) != 1 || set.Data[0].Port != "value" || set.Data[0].From != "v" {
		t.Fatalf("set data round-trip = %+v", set.Data)
	}

	// And the decoded program installs onto a real Scene without panic —
	// the runtime accepts the compiler's artefact end to end (it does NOT
	// fire here; emit-but-never-install means activation is issue #106).
	sc := NewScene("scene-exec", graph, nil, NewComputeRegistry(), quietLogger())
	sc.InstallExec(p)
}
