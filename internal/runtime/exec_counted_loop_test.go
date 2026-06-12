package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Regression for the counted-loop iteration bug found live (forge/
// orion-counted-loop-iteration): `core.flow.for-loop@1` and
// `core.flow.for-each@1` did not iterate their body at the antenna —
// the accumulator stayed at its reset value because the body was pushed
// ZERO times.
//
// ROOT CAUSE (compiler, exec_partition.go::buildExecNode): Blue's seed
// declares a counted loop's control inputs as DATA pins carrying a
// `default` — `for-loop`'s first/last (_data_in(..., default=0)) and
// `for-each`'s items — never as config keys (the seed's signature.config
// is empty). When the author types an inline literal instead of wiring a
// producer edge, that value lives on the input PORT's `default`. The
// data layer already seeds graph.Defaults from `p.Default`, but the exec
// partition carried ONLY n.Config, dropping the defaults. So a literal-
// bounded for-loop reached the interpreter with no first/last; pullInt
// fell to its def (-1 for `last`) → `0 <= -1` is false → ZERO iterations.
// `while` was immune (its condition is always a wired comparison, never
// a literal default) — which is exactly why it worked live while counted
// loops froze.
//
// These tests drive the REAL compiler (compiler.Compile) on the LIVE
// graph shape (literal bounds via port defaults, NO wired first/last/
// items edge), then run the compiled artefact through a live Scene. They
// assert the body actually iterated by checking the accumulator: a
// for-loop 0..6 summing its index → 21, a for-each [10,20,30] folding
// its element → 60. A green run proves the body is pushed once per
// iteration with the index/element bound, to termination.

func execPort(name string) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: name, Type: "exec", Kind: "exec"}
}

func dataPort(name string) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: name, Type: "any", Kind: "data"}
}

// dataPortDefault is a DATA input port carrying an inline literal — the
// exact authoring shape that broke counted loops (a typed bound, no
// wired producer edge).
func dataPortDefault(name string, def json.RawMessage) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: name, Type: "any", Kind: "data", Default: def}
}

// loopAccManifest covers every compute the accumulator round-trip and the
// loop control nodes reference.
func loopAccManifest() compiler.ComputeManifest {
	return compiler.ComputeManifest{
		"core.event.on-start@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.flow.for-loop@1":  {IsPure: true, IsBounded: true, Version: "1"},
		"core.flow.for-each@1":  {IsPure: true, IsBounded: true, Version: "1"},
		"core.variable.get@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.variable.set@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.math.add@1":       {IsPure: true, IsBounded: true, Version: "1"},
	}
}

// runCompiledExecScene compiles a blueprint with the real compiler, decodes
// its exec program, installs it into a live Scene and fires every on-start
// entrypoint. Returns the running scene (cleanup registered by the caller's t).
func runCompiledExecScene(t *testing.T, bp *compiler.BlueprintGraph) *Scene {
	t.Helper()
	f := &stubFetcher{
		layout:    &compiler.CanvasLayout{Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		blueprint: bp,
		manifest:  loopAccManifest(),
	}
	graph, bundle, _, err := compiler.Compile(context.Background(), "loop-scene",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: bp.ID}, f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	progs, err := ExecProgramsFromGraph(graph)
	if err != nil || len(progs) == 0 {
		t.Fatalf("exec programs: %v (n=%d)", err, len(progs))
	}

	sc := NewScene("loop-scene", graph, bundle, NewComputeRegistry(), quietLogger())
	for _, p := range progs {
		sc.InstallExec(p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sc.Run(ctx)
	t.Cleanup(sc.Stop)

	for _, p := range progs {
		for name, e := range p.Entrypoints {
			if e.Kind == EntryOnStart {
				mustFire(t, sc, name)
			}
		}
	}
	return sc
}

// TestExec_CountedForLoop_IteratesBody_LiteralBounds is the live-shape
// regression: on-start → for-loop(first=0,last=6 via PORT DEFAULTS) with
// body get(acc) → add(index) → set(acc). The accumulator must reach
// 0+1+2+3+4+5+6 = 21. Before the fix it stayed at 0 (body never pushed).
func TestExec_CountedForLoop_IteratesBody_LiteralBounds(t *testing.T) {
	bp := &compiler.BlueprintGraph{
		ID: "bp-forloop",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{execPort("then")}},
			// first/last carried as INLINE LITERAL port defaults — the
			// authoring shape that broke (no wired bound edge).
			{ID: "loop", Compute: "core.flow.for-loop@1",
				Inputs: []compiler.BlueprintPort{
					execPort("in"),
					dataPortDefault("first", raw(`0`)),
					dataPortDefault("last", raw(`6`)),
				},
				Outputs: []compiler.BlueprintPort{
					execPort("body"), dataPort("index"), execPort("completed"),
				}},
			{ID: "get", Compute: "core.variable.get@1",
				Config:  map[string]json.RawMessage{"variable": raw(`"acc"`)},
				Outputs: []compiler.BlueprintPort{dataPort("out")}},
			{ID: "add", Compute: "core.math.add@1",
				Inputs:  []compiler.BlueprintPort{dataPort("a"), dataPort("b")},
				Outputs: []compiler.BlueprintPort{dataPort("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": raw(`"acc"`)},
				Inputs:  []compiler.BlueprintPort{execPort("exec_in"), dataPort("value")},
				Outputs: []compiler.BlueprintPort{execPort("then")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "loop", ToPort: "in"},
			{FromNode: "loop", FromPort: "body", ToNode: "set", ToPort: "exec_in"},
			// acc round-trip: get(acc) + index → set(acc).
			{FromNode: "get", FromPort: "out", ToNode: "add", ToPort: "a"},
			{FromNode: "loop", FromPort: "index", ToNode: "add", ToPort: "b"},
			{FromNode: "add", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}

	sc := runCompiledExecScene(t, bp)
	waitForState(t, sc, "__vars..acc", "21", time.Second)
}

// TestExec_CountedForEach_IteratesBody_LiteralItems: on-start →
// for-each(items=[10,20,30] via PORT DEFAULT) with body get(acc) →
// add(element) → set(acc). The accumulator must reach 10+20+30 = 60.
// Before the fix it stayed at 0 (items dropped, body never pushed).
func TestExec_CountedForEach_IteratesBody_LiteralItems(t *testing.T) {
	bp := &compiler.BlueprintGraph{
		ID: "bp-foreach",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{execPort("then")}},
			{ID: "each", Compute: "core.flow.for-each@1",
				Inputs: []compiler.BlueprintPort{
					execPort("in"),
					dataPortDefault("items", raw(`[10,20,30]`)),
				},
				Outputs: []compiler.BlueprintPort{
					execPort("body"), dataPort("element"), dataPort("index"), execPort("completed"),
				}},
			{ID: "get", Compute: "core.variable.get@1",
				Config:  map[string]json.RawMessage{"variable": raw(`"acc"`)},
				Outputs: []compiler.BlueprintPort{dataPort("out")}},
			{ID: "add", Compute: "core.math.add@1",
				Inputs:  []compiler.BlueprintPort{dataPort("a"), dataPort("b")},
				Outputs: []compiler.BlueprintPort{dataPort("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": raw(`"acc"`)},
				Inputs:  []compiler.BlueprintPort{execPort("exec_in"), dataPort("value")},
				Outputs: []compiler.BlueprintPort{execPort("then")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "each", ToPort: "in"},
			{FromNode: "each", FromPort: "body", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "get", FromPort: "out", ToNode: "add", ToPort: "a"},
			{FromNode: "each", FromPort: "element", ToNode: "add", ToPort: "b"},
			{FromNode: "add", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}

	sc := runCompiledExecScene(t, bp)
	waitForState(t, sc, "__vars..acc", "60", time.Second)
}
