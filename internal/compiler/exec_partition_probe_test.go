package compiler

// Probe tests for the exec-partition lift (ADR 006 §3.1/§3.2, issue #103).
// These complement Forge's exec_partition_test.go without rewriting it.
// Each case is independently reproducible and asserts a real invariant.
//
// Coverage targets:
//  1. Silent-skip closed: is_pure:true nodes WITH exec pins (branch, sequence,
//     loop) never appear as data GraphNodes.
//  2. Round-trip shapes Forge did not cover: nested loop, sequence-in-branch,
//     multiple on-event topics, delay.
//  3. Pure byte-identical: a pure-dataflow scene produces no exec_programs key.
//  4. Determinism: N compiles of a complex exec scene are byte-identical.
//  5. EXEC_OP_UNMAPPED: compile-level fail-loud, not a crash.
//  6. Multi-blueprint: 2 exec blueprints → 2 programs, key order deterministic.
//  7. on-event config key contract: Blue uses "event_name" not "event" — defect.
//  8. Dormant: scenes_push.go does not call InstallExec or LoadExec.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fullExecManifest extends execManifest with the loop/while/for-each ops
// Forge's execManifest omits.
func fullExecManifest() ComputeManifest {
	m := execManifest()
	for _, id := range []string{
		"core.flow.for-each@1", "core.flow.while@1",
		"core.flow.gate@1",
	} {
		m[id] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	}
	return m
}

// compileExecScene compiles a single-blueprint scene with fullExecManifest.
func compileExecScene(t *testing.T, bp *BlueprintGraph) (*Graph, error) {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{bp.ID: bp},
		manifest:   fullExecManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: bp.ID}, f)
	return g, err
}

// ---------------------------------------------------------------------------
// 1. Silent-skip closed — is_pure:true exec-pin nodes leave the data graph
// ---------------------------------------------------------------------------

// TestPartitionProbe_PureExecNodes_NotInDataGraph exercises each exec-pin
// "pure" flow node (branch, sequence, for-loop, for-each, while) and proves
// none appears as a data GraphNode. Before #103 they fell through as data nodes
// with no compute executor (silent skip). The discriminator is the exec PIN,
// not is_pure.
func TestPartitionProbe_PureExecNodes_NotInDataGraph(t *testing.T) {
	cases := []struct {
		name    string
		compute string
		inputs  []BlueprintPort
		outputs []BlueprintPort
	}{
		{
			name:    "branch",
			compute: "core.flow.branch@1",
			inputs:  []BlueprintPort{execIn("exec_in"), dataIn("condition")},
			outputs: []BlueprintPort{execOut("true"), execOut("false")},
		},
		{
			name:    "sequence",
			compute: "core.flow.sequence@1",
			inputs:  []BlueprintPort{execIn("in")},
			outputs: []BlueprintPort{execOut("then_0"), execOut("then_1")},
		},
		{
			name:    "for-loop",
			compute: "core.flow.for-loop@1",
			inputs:  []BlueprintPort{execIn("exec_in")},
			outputs: []BlueprintPort{execOut("body"), execOut("completed")},
		},
		{
			name:    "for-each",
			compute: "core.flow.for-each@1",
			inputs:  []BlueprintPort{execIn("exec_in"), dataIn("list")},
			outputs: []BlueprintPort{execOut("body"), execOut("completed")},
		},
		{
			name:    "while",
			compute: "core.flow.while@1",
			inputs:  []BlueprintPort{execIn("exec_in")},
			outputs: []BlueprintPort{execOut("body"), execOut("completed")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bp := &BlueprintGraph{
				ID: "bp-1",
				Nodes: []BlueprintNode{
					{
						ID:      "start",
						Compute: "core.event.on-start@1",
						Outputs: []BlueprintPort{execOut("then")},
					},
					{
						ID:      "node",
						Compute: tc.compute,
						Inputs:  tc.inputs,
						Outputs: tc.outputs,
					},
				},
				Edges: []BlueprintEdge{
					{FromNode: "start", FromPort: "then", ToNode: "node", ToPort: "exec_in"},
				},
			}
			g, err := compileExecScene(t, bp)
			if err != nil {
				t.Fatalf("compile error: %v", err)
			}
			// The exec node must NOT appear in the data graph.
			if _, ok := graphNodeByID(g, "node"); ok {
				t.Fatalf("%s: exec node leaked into data GraphNode list — silent-skip NOT closed", tc.name)
			}
			// The entry (on-start) must NOT appear in data graph.
			if _, ok := graphNodeByID(g, "start"); ok {
				t.Fatalf("%s: entry node leaked into data GraphNode list", tc.name)
			}
			// One exec program emitted.
			if len(g.ExecPrograms) != 1 {
				t.Fatalf("%s: want 1 exec program, got %d", tc.name, len(g.ExecPrograms))
			}
			// The program must carry the node (not just the entry).
			p := decodeProgram(t, g.ExecPrograms[0])
			if _, ok := p.Nodes["node"]; !ok {
				t.Fatalf("%s: exec node absent from program nodes %v", tc.name, p.Nodes)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2a. Round-trip: sequence nested inside branch
// ---------------------------------------------------------------------------

// TestPartitionProbe_SequenceInBranch: on-start → branch → [true: sequence
// → {then_0: set1, then_1: set2}]. Proves sequence ops with multiple then_N
// pins and branch exec edges all route to the exec program, with correct Next.
func TestPartitionProbe_SequenceInBranch(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "cond", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`true`)},
				Outputs: []BlueprintPort{dataIn("out")}},
			{ID: "br", Compute: "core.flow.branch@1",
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("condition")},
				Outputs: []BlueprintPort{execOut("true"), execOut("false")}},
			{ID: "seq", Compute: "core.flow.sequence@1",
				Inputs:  []BlueprintPort{execIn("in")},
				Outputs: []BlueprintPort{execOut("then_0"), execOut("then_1"), execOut("then_2")}},
			{ID: "set1", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"a"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "set2", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"b"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "br", ToPort: "exec_in"},
			{FromNode: "cond", FromPort: "out", ToNode: "br", ToPort: "condition"},
			{FromNode: "br", FromPort: "true", ToNode: "seq", ToPort: "in"},
			{FromNode: "seq", FromPort: "then_0", ToNode: "set1", ToPort: "exec_in"},
			{FromNode: "seq", FromPort: "then_1", ToNode: "set2", ToPort: "exec_in"},
		},
	}

	g, err := compileExecScene(t, bp)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	// No exec node in the data graph.
	for _, id := range []string{"start", "br", "seq", "set1", "set2"} {
		if _, ok := graphNodeByID(g, id); ok {
			t.Fatalf("exec node %q leaked into data graph", id)
		}
	}
	// Literal feeding the branch condition stays data.
	if _, ok := graphNodeByID(g, "cond"); !ok {
		t.Fatal("data literal cond dropped from data graph")
	}

	p := decodeProgram(t, g.ExecPrograms[0])

	// branch wired: Next["true"] → seq.
	br, ok := p.Nodes["br"]
	if !ok {
		t.Fatal("branch node missing from exec program")
	}
	if tgt, ok := br.Next["true"]; !ok || tgt.Node != "seq" {
		t.Fatalf("branch Next[true] = %+v, want seq", br.Next)
	}
	// sequence has Next[then_0] → set1, Next[then_1] → set2.
	seq, ok := p.Nodes["seq"]
	if !ok {
		t.Fatal("sequence node missing from exec program")
	}
	if tgt := seq.Next["then_0"]; tgt.Node != "set1" {
		t.Fatalf("seq Next[then_0] = %+v, want set1", seq.Next)
	}
	if tgt := seq.Next["then_1"]; tgt.Node != "set2" {
		t.Fatalf("seq Next[then_1] = %+v, want set2", seq.Next)
	}
}

// ---------------------------------------------------------------------------
// 2b. Round-trip: for-loop with exec-producer data out (index pin)
// ---------------------------------------------------------------------------

// TestPartitionProbe_ForLoopIndexPin: on-start → for-loop → body: print.
// The print node's value is fed from the loop's index pin (exec-producer data
// out). Confirms ExecDataInput.FromPort is carried verbatim and the From is
// NOT key-prefixed (exec-producer is within the program).
func TestPartitionProbe_ForLoopIndexPin(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "loop", Compute: "core.flow.for-loop@1",
				Config:  map[string]json.RawMessage{"first": json.RawMessage(`0`), "last": json.RawMessage(`3`)},
				Inputs:  []BlueprintPort{execIn("exec_in")},
				Outputs: []BlueprintPort{execOut("body"), execOut("completed"), dataIn("index")}},
			{ID: "pr", Compute: "core.print@1",
				Config: map[string]json.RawMessage{"message": json.RawMessage(`"i"`)},
				Inputs: []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "loop", ToPort: "exec_in"},
			{FromNode: "loop", FromPort: "body", ToNode: "pr", ToPort: "exec_in"},
			// data edge: loop's "index" output (exec-producer data out) → print's "value" input.
			{FromNode: "loop", FromPort: "index", ToNode: "pr", ToPort: "value"},
		},
	}

	g, err := compileExecScene(t, bp)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	p := decodeProgram(t, g.ExecPrograms[0])

	pr, ok := p.Nodes["pr"]
	if !ok {
		t.Fatal("print node missing from exec program")
	}
	// ExecDataInput.From must NOT be key-prefixed (exec-producer inside
	// the same program). For a keyed blueprint this would be "loop",
	// never "key.loop".
	if len(pr.Data) != 1 {
		t.Fatalf("print data = %+v, want 1 entry", pr.Data)
	}
	if pr.Data[0].From != "loop" || pr.Data[0].FromPort != "index" {
		t.Fatalf("print data[0] = %+v, want {value, loop, index}", pr.Data[0])
	}
	// Loop's body edge wires to pr as exec-pin.
	loop, ok := p.Nodes["loop"]
	if !ok {
		t.Fatal("loop node missing from exec program")
	}
	if tgt := loop.Next["body"]; tgt.Node != "pr" {
		t.Fatalf("loop Next[body] = %+v, want pr", loop.Next)
	}
}

// ---------------------------------------------------------------------------
// 2c. Round-trip: multiple on-event topics
// ---------------------------------------------------------------------------

// TestPartitionProbe_MultipleOnEvent: two on-event nodes with different
// topics compile to two entries with correct Kind, Event fields, and distinct
// Target nodes. Confirms multi-entry programs.
func TestPartitionProbe_MultipleOnEvent(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			// on-event "goal" → set1.
			// Use BOTH keys so the test passes regardless of which key
			// the compiler currently reads (the correct key is "event_name" —
			// see TestPartitionProbe_OnEvent_ConfigKeyContract).
			{
				ID:      "ev_goal",
				Compute: "core.event.on-event@1",
				Config: map[string]json.RawMessage{
					"event":      json.RawMessage(`"goal"`),
					"event_name": json.RawMessage(`"goal"`),
				},
				Outputs: []BlueprintPort{execOut("then")},
			},
			{ID: "set1", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"goals"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
			// on-event "assist" → set2.
			{
				ID:      "ev_assist",
				Compute: "core.event.on-event@1",
				Config: map[string]json.RawMessage{
					"event":      json.RawMessage(`"assist"`),
					"event_name": json.RawMessage(`"assist"`),
				},
				Outputs: []BlueprintPort{execOut("then")},
			},
			{ID: "set2", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"assists"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "ev_goal", FromPort: "then", ToNode: "set1", ToPort: "exec_in"},
			{FromNode: "ev_assist", FromPort: "then", ToNode: "set2", ToPort: "exec_in"},
		},
	}

	g, err := compileExecScene(t, bp)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	p := decodeProgram(t, g.ExecPrograms[0])

	if len(p.Entrypoints) != 2 {
		t.Fatalf("want 2 entrypoints, got %d: %+v", len(p.Entrypoints), p.Entrypoints)
	}

	eGoal, ok := p.Entrypoints["ev_goal"]
	if !ok {
		t.Fatal("entry ev_goal missing")
	}
	if eGoal.Kind != "on-event" {
		t.Fatalf("ev_goal kind = %q, want on-event", eGoal.Kind)
	}
	if eGoal.Event != "goal" {
		t.Fatalf("ev_goal event = %q, want goal", eGoal.Event)
	}
	if eGoal.Target.Node != "set1" {
		t.Fatalf("ev_goal target = %+v, want set1", eGoal.Target)
	}

	eAssist, ok := p.Entrypoints["ev_assist"]
	if !ok {
		t.Fatal("entry ev_assist missing")
	}
	if eAssist.Kind != "on-event" {
		t.Fatalf("ev_assist kind = %q, want on-event", eAssist.Kind)
	}
	if eAssist.Event != "assist" {
		t.Fatalf("ev_assist event = %q, want assist", eAssist.Event)
	}
	if eAssist.Target.Node != "set2" {
		t.Fatalf("ev_assist target = %+v, want set2", eAssist.Target)
	}
}

// ---------------------------------------------------------------------------
// 2d. Round-trip: delay node config verbatim
// ---------------------------------------------------------------------------

// TestPartitionProbe_DelayConfigVerbatim: on-start → delay (seconds=2.5) →
// set. Confirms delay is routed to exec program, config carried verbatim,
// and Next["then"] wires to set.
func TestPartitionProbe_DelayConfigVerbatim(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "d", Compute: "core.flow.delay@1",
				Config:  map[string]json.RawMessage{"seconds": json.RawMessage(`2.5`)},
				Inputs:  []BlueprintPort{execIn("exec_in")},
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"fired"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "d", ToPort: "exec_in"},
			{FromNode: "d", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
		},
	}

	g, err := compileExecScene(t, bp)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	p := decodeProgram(t, g.ExecPrograms[0])

	delay, ok := p.Nodes["d"]
	if !ok {
		t.Fatal("delay node missing from program")
	}
	if delay.Op != "delay" {
		t.Fatalf("delay op = %q, want delay", delay.Op)
	}
	if string(delay.Config["seconds"]) != `2.5` {
		t.Fatalf("delay config.seconds = %s, want 2.5 (verbatim)", delay.Config["seconds"])
	}
	if tgt := delay.Next["then"]; tgt.Node != "set" {
		t.Fatalf("delay Next[then] = %+v, want set", delay.Next)
	}
}

// ---------------------------------------------------------------------------
// 3. Pure-dataflow byte-identical: no exec_programs key in JSON
// ---------------------------------------------------------------------------

// TestPartitionProbe_PureDataflow_NoExecPrograms_AltBlueprint: a pure-only
// blueprint produces no ExecPrograms and no exec_programs key in JSON.
// This is a different blueprint from Forge's golden test (add two literals)
// to prove the invariant is not layout-specific.
func TestPartitionProbe_PureDataflow_NoExecPrograms_AltBlueprint(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-pure",
		Nodes: []BlueprintNode{
			{ID: "lit1", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`10`)},
				Outputs: []BlueprintPort{dataIn("out")}},
			{ID: "lit2", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`20`)},
				Outputs: []BlueprintPort{dataIn("out")}},
			{ID: "add", Compute: "core.math.add@1",
				Inputs:  []BlueprintPort{dataIn("x"), dataIn("y")},
				Outputs: []BlueprintPort{dataIn("out")}},
			outputNode("out", "score.total"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "lit1", FromPort: "out", ToNode: "add", ToPort: "x"},
			{FromNode: "lit2", FromPort: "out", ToNode: "add", ToPort: "y"},
			{FromNode: "add", FromPort: "out", ToNode: "out", ToPort: "value"},
		},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-pure": bp},
		manifest:   pureManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-pure"}, f)
	if err != nil {
		t.Fatalf("pure scene compile error: %v", err)
	}

	if g.ExecPrograms != nil {
		t.Fatalf("pure scene must not carry exec programs, got %d", len(g.ExecPrograms))
	}
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatal(err)
	}
	if _, present := asMap["exec_programs"]; present {
		t.Fatal("pure scene graph JSON must not contain exec_programs key (byte-identity broken)")
	}
}

// ---------------------------------------------------------------------------
// 4. Determinism: N compiles of a complex exec scene are byte-identical
// ---------------------------------------------------------------------------

// TestPartitionProbe_Determinism_Complex: a blueprint with event, branch,
// sequence, delay, two variable.sets, and a data literal. Compile 10 times;
// assert raw JSON byte-identical.
func TestPartitionProbe_Determinism_Complex(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-complex",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "condlit", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`false`)},
				Outputs: []BlueprintPort{dataIn("out")}},
			{ID: "br", Compute: "core.flow.branch@1",
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("condition")},
				Outputs: []BlueprintPort{execOut("true"), execOut("false")}},
			{ID: "seq", Compute: "core.flow.sequence@1",
				Inputs:  []BlueprintPort{execIn("in")},
				Outputs: []BlueprintPort{execOut("then_0"), execOut("then_1")}},
			{ID: "d", Compute: "core.flow.delay@1",
				Config:  map[string]json.RawMessage{"seconds": json.RawMessage(`1`)},
				Inputs:  []BlueprintPort{execIn("exec_in")},
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "setA", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"z"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "setB", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"w"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "vallit", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`99`)},
				Outputs: []BlueprintPort{dataIn("out")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "br", ToPort: "exec_in"},
			{FromNode: "condlit", FromPort: "out", ToNode: "br", ToPort: "condition"},
			{FromNode: "br", FromPort: "true", ToNode: "seq", ToPort: "in"},
			{FromNode: "br", FromPort: "false", ToNode: "d", ToPort: "exec_in"},
			{FromNode: "seq", FromPort: "then_0", ToNode: "setA", ToPort: "exec_in"},
			{FromNode: "seq", FromPort: "then_1", ToNode: "setB", ToPort: "exec_in"},
			{FromNode: "d", FromPort: "then", ToNode: "setA", ToPort: "exec_in"},
			{FromNode: "vallit", FromPort: "out", ToNode: "setA", ToPort: "value"},
			{FromNode: "vallit", FromPort: "out", ToNode: "setB", ToPort: "value"},
		},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-complex": bp},
		manifest:   fullExecManifest(),
	}
	env := PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-complex"}

	var first []byte
	for i := 0; i < 10; i++ {
		g, _, _, err := Compile(context.Background(), "scene-1", env, f)
		if err != nil {
			t.Fatalf("compile %d error: %v", i, err)
		}
		raw, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = raw
			continue
		}
		if string(raw) != string(first) {
			t.Fatalf("compile %d produced a different artefact — not byte-identical\ngot:  %s\nwant: %s", i, raw, first)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. EXEC_OP_UNMAPPED — fail-loud, not a crash
// ---------------------------------------------------------------------------

// TestPartitionProbe_ExecOpUnmapped_NotCrash: a node with exec pins whose
// manifest entry maps to no op → compile returns EXEC_OP_UNMAPPED (not nil
// err, not a panic, not a silent omission from the program).
func TestPartitionProbe_ExecOpUnmapped_NotCrash(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{ID: "mystery", Compute: "core.flow.mystery@99",
				Inputs:  []BlueprintPort{execIn("exec_in")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
	}
	m := fullExecManifest()
	// In the manifest (passes unknown-compute gate) but no conformance mapping.
	m["core.flow.mystery@99"] = ComputeManifestEntry{IsPure: true, Version: "1"}

	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		manifest:   m,
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err == nil {
		t.Fatal("want EXEC_OP_UNMAPPED error, compile succeeded")
	}
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrExecOpUnmapped) {
		t.Fatalf("want EXEC_OP_UNMAPPED, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 6. Multi-blueprint: 2 exec blueprints → 2 programs, deterministic key order
// ---------------------------------------------------------------------------

// TestPartitionProbe_MultiBlueprint_TwoExecPrograms: two blueprints both
// carrying exec nodes produce exactly 2 ExecPrograms. Their order in
// graph.ExecPrograms follows the stable blueprint-key sort order (ADR 001
// §3.2), and each program's BlueprintKey is its scene-local key.
func TestPartitionProbe_MultiBlueprint_TwoExecPrograms(t *testing.T) {
	bpAlpha := &BlueprintGraph{
		ID: "bp-alpha",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "pr", Compute: "core.print@1",
				Config: map[string]json.RawMessage{"message": json.RawMessage(`"alpha"`)},
				Inputs: []BlueprintPort{execIn("exec_in")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "pr", ToPort: "exec_in"},
		},
	}
	bpZeta := &BlueprintGraph{
		ID: "bp-zeta",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"x"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
		},
	}
	f := &fakeFetcher{
		layouts: map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{
			"bp-alpha": bpAlpha,
			"bp-zeta":  bpZeta,
		},
		manifest: fullExecManifest(),
	}
	// Push with keys "alpha" < "zeta" — authored out of order to exercise sorting.
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{
			{Key: "zeta", ID: "bp-zeta"},   // authored out of sort order
			{Key: "alpha", ID: "bp-alpha"}, // should sort first
		}}, f)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}
	if len(g.ExecPrograms) != 2 {
		t.Fatalf("want 2 exec programs, got %d", len(g.ExecPrograms))
	}

	p0 := decodeProgram(t, g.ExecPrograms[0])
	p1 := decodeProgram(t, g.ExecPrograms[1])

	// Stable key order: "alpha" < "zeta".
	if p0.BlueprintKey != "alpha" {
		t.Fatalf("programs[0].BlueprintKey = %q, want alpha (sorted first)", p0.BlueprintKey)
	}
	if p1.BlueprintKey != "zeta" {
		t.Fatalf("programs[1].BlueprintKey = %q, want zeta (sorted second)", p1.BlueprintKey)
	}

	// Each program is self-contained: exec node ids are unprefixed inside.
	if _, ok := p0.Nodes["pr"]; !ok {
		t.Fatalf("alpha program missing node pr; nodes = %v", p0.Nodes)
	}
	if _, ok := p1.Nodes["set"]; !ok {
		t.Fatalf("zeta program missing node set; nodes = %v", p1.Nodes)
	}

	// No exec node from either blueprint leaked into the data graph.
	for _, key := range []string{"alpha", "zeta"} {
		for _, id := range []string{"start", "pr", "set"} {
			prefixed := key + "." + id
			if _, ok := graphNodeByID(g, prefixed); ok {
				t.Fatalf("exec node %q leaked into data graph", prefixed)
			}
		}
	}
}

// TestPartitionProbe_MultiBlueprint_Deterministic: same 2-blueprint scene,
// 5 compiles → byte-identical artefact each time.
func TestPartitionProbe_MultiBlueprint_Deterministic(t *testing.T) {
	bpAlpha := &BlueprintGraph{
		ID: "bp-alpha",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1", Outputs: []BlueprintPort{execOut("then")}},
			{ID: "pr", Compute: "core.print@1",
				Config: map[string]json.RawMessage{"message": json.RawMessage(`"a"`)},
				Inputs: []BlueprintPort{execIn("exec_in")}, Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{{FromNode: "start", FromPort: "then", ToNode: "pr", ToPort: "exec_in"}},
	}
	bpZeta := &BlueprintGraph{
		ID: "bp-zeta",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1", Outputs: []BlueprintPort{execOut("then")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"x"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{{FromNode: "start", FromPort: "then", ToNode: "set", ToPort: "exec_in"}},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-alpha": bpAlpha, "bp-zeta": bpZeta},
		manifest:   fullExecManifest(),
	}
	env := PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{
		{Key: "alpha", ID: "bp-alpha"},
		{Key: "zeta", ID: "bp-zeta"},
	}}
	var first []byte
	for i := 0; i < 5; i++ {
		g, _, _, err := Compile(context.Background(), "scene-1", env, f)
		if err != nil {
			t.Fatalf("compile %d error: %v", i, err)
		}
		raw, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = raw
			continue
		}
		if string(raw) != string(first) {
			t.Fatalf("multi-blueprint compile %d diverged — not byte-identical", i)
		}
	}
}

// ---------------------------------------------------------------------------
// 7. on-event config key — DEFECT cross-repo contract divergence
// ---------------------------------------------------------------------------

// TestPartitionProbe_OnEvent_ConfigKeyContract verifies the cross-repo contract
// between Blue's on-event config schema and Orion's compiler.
//
// Blue's stdlib_seeder.py declares the on-event config param as "event_name"
// (line 122: {"name": "event_name", ...}).
// execEntryEventConfigKey = "event_name" (exec_partition.go:66) must match.
//
// A blueprint with only config.event_name (the Blue canonical key) must compile
// successfully and produce an ExecEntry with Kind=="on-event" and Event=="goal".
// Any regression to "event" would cause EXEC_OP_UNMAPPED on all on-event pushes.
func TestPartitionProbe_OnEvent_ConfigKeyContract(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{
				ID:      "ev",
				Compute: "core.event.on-event@1",
				// Only "event_name" — the Blue canonical key (stdlib_seeder.py:122).
				Config:  map[string]json.RawMessage{"event_name": json.RawMessage(`"goal"`)},
				Outputs: []BlueprintPort{execOut("then")},
			},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"g"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "ev", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
		},
	}

	g, err := compileExecScene(t, bp)
	if err != nil {
		t.Fatalf("compile error: %v — execEntryEventConfigKey must be \"event_name\" to match Blue contract (stdlib_seeder.py:122)", err)
	}

	p := decodeProgram(t, g.ExecPrograms[0])

	entry, ok := p.Entrypoints["ev"]
	if !ok {
		t.Fatalf("entry \"ev\" missing from exec program; entrypoints = %+v", p.Entrypoints)
	}
	if entry.Kind != "on-event" {
		t.Fatalf("entry.Kind = %q, want \"on-event\"", entry.Kind)
	}
	if entry.Event != "goal" {
		t.Fatalf("entry.Event = %q, want \"goal\" — config.event_name not read correctly", entry.Event)
	}
	if entry.Target.Node != "set" {
		t.Fatalf("entry.Target.Node = %q, want \"set\"", entry.Target.Node)
	}
}

// ---------------------------------------------------------------------------
// 8. R9 lift (issue #106): scenes_push.go installs exec ONLY via execForAir
// ---------------------------------------------------------------------------

// TestPartitionProbe_PushInstallsExecThroughGate: a structural source-level
// check that, post-R9-lift (ADR 006 §3.4, issue #106), the production
// scenes_push.go handler installs exec — but ONLY through the gated seam
// execForAir, never by resolving programs itself (ExecProgramsFromGraph)
// and never by handing programs to LoadExec down a path that skips the
// validation gate. The invariant is: every install is keyed on the #87
// validation record, which execForAir is the single composer of.
//
// This SUPERSEDES the pre-lift dormancy guard (exec was dormant until
// #106): the lift's whole point is that a VALIDATED scene installs its
// exec. The guard now protects the franchissement's safety property — no
// bypass of the gate — rather than its dormancy.
//
// The source is read at test run time from the adjacent api/ package using
// runtime.Caller to anchor the path correctly regardless of test working dir.
func TestPartitionProbe_PushInstallsExecThroughGate(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("runtime.Caller failed — cannot locate scenes_push.go")
	}
	// thisFile is .../internal/compiler/exec_partition_probe_test.go
	// scenes_push.go is at .../internal/api/scenes_push.go
	pushPath := filepath.Join(filepath.Dir(thisFile), "..", "api", "scenes_push.go")
	src, err := os.ReadFile(pushPath)
	if err != nil {
		t.Skipf("cannot read scenes_push.go (%v) — gate check skipped", err)
	}
	content := string(src)

	// The lift requires the push path to install exec through execForAir.
	if !strings.Contains(content, "execForAir") {
		t.Error("scenes_push.go no longer calls execForAir — the R9 lift's gated install seam is missing (ADR 006 §3.4)")
	}
	// The push handler must NEVER resolve programs itself: that would be a
	// path around the validation gate. ExecProgramsFromGraph is only legal
	// INSIDE execForAir (gate.go), which gates it on the validation record.
	if strings.Contains(content, "ExecProgramsFromGraph") {
		t.Error("scenes_push.go calls ExecProgramsFromGraph directly — exec install must go through execForAir, never around the #87 gate")
	}
}
