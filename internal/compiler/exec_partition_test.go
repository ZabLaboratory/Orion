package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// execManifest is pureManifest plus the exec ops and event entrypoints
// this file exercises. Every exec node is is_pure:true (Blue's reality:
// flow nodes are pure but exec) — proving the discriminator is the exec
// PIN, not purity.
func execManifest() ComputeManifest {
	m := pureManifest()
	for _, id := range []string{
		"core.event.on-start@1", "core.event.on-tick@1", "core.event.on-event@1",
		"core.flow.branch@1", "core.flow.for-loop@1", "core.flow.sequence@1",
		"core.variable.set@1", "core.print@1", "core.flow.delay@1",
	} {
		m[id] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	}
	return m
}

func execIn(name string) BlueprintPort  { return BlueprintPort{Name: name, Type: "exec", Kind: "exec"} }
func execOut(name string) BlueprintPort { return BlueprintPort{Name: name, Type: "exec", Kind: "exec"} }
func dataIn(name string) BlueprintPort  { return BlueprintPort{Name: name, Type: "any", Kind: "data"} }

// decodeProgram reads one emitted ExecPrograms entry back into the
// compiler-side mirror so structure can be asserted without importing
// the runtime (the runtime round-trip is proven in the runtime package,
// criterion #1).
func decodeProgram(t *testing.T, raw json.RawMessage) execProgram {
	t.Helper()
	var p execProgram
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode emitted program: %v", err)
	}
	return p
}

// mixedExecBlueprint: on-start → branch → variable.set, with a literal
// feeding the branch condition (data edge into an exec pin) and the
// set's value (data edge into an exec pin). Mixes pure data, pure
// exec-flow (branch) and effect exec (variable.set) — ADR 006 §6 #1.
func mixedExecBlueprint() *BlueprintGraph {
	return &BlueprintGraph{
		ID: "bp-mixed",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "cond", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`true`)},
				Outputs: []BlueprintPort{dataIn("out")}},
			{ID: "br", Compute: "core.flow.branch@1",
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("condition")},
				Outputs: []BlueprintPort{execOut("true"), execOut("false")}},
			{ID: "val", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`9`)},
				Outputs: []BlueprintPort{dataIn("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"counter"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "br", ToPort: "exec_in"},
			{FromNode: "cond", FromPort: "out", ToNode: "br", ToPort: "condition"},
			{FromNode: "br", FromPort: "true", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "val", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}
}

func compileMixed(t *testing.T) *Graph {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": mixedExecBlueprint()},
		manifest:   execManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatalf("mixed exec scene rejected: %v", err)
	}
	return g
}

// Criterion #1 (structure half): exec nodes leave the data graph, the
// event node becomes an entry, the exec edge becomes Next, the data
// edges into exec pins become ExecDataInput. The pure literal that only
// feeds the data layer stays a data node.
// reactiveOnEventBlueprint mirrors the Quasar-finale reactive scene
// (ADR 013): an on-event entry whose ONLY out-edge is a DATA edge
// (`payload` → a get-field's data input), feeding a pure dataflow chain
// into a core.output@1 sink. There is no exec body. The entry must
// therefore carry NO exec Target — the reactivity is the __events write
// re-evaluating the data chain, not an exec dispatch. Regression for the
// compiler wiring the entry Target off a DATA out-edge, which made the
// runtime walk into a data node ("exec: unknown node id").
func reactiveOnEventBlueprint() *BlueprintGraph {
	return &BlueprintGraph{
		ID: "bp-reactive",
		Nodes: []BlueprintNode{
			{ID: "onChat", Compute: "core.event.on-event@1",
				Config:  map[string]json.RawMessage{"event_name": json.RawMessage(`"stream_chat_event"`)},
				Outputs: []BlueprintPort{execOut("then"), dataIn("payload")}},
			{ID: "text", Compute: "core.data.get-field@1",
				Config:  map[string]json.RawMessage{"path": json.RawMessage(`"payload.text"`)},
				Inputs:  []BlueprintPort{dataIn("record")},
				Outputs: []BlueprintPort{dataIn("value")}},
			{ID: "out", Compute: "core.output@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"chat.display"`)},
				Inputs: []BlueprintPort{dataIn("value")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "onChat", FromPort: "payload", ToNode: "text", ToPort: "record"},
			{FromNode: "text", FromPort: "value", ToNode: "out", ToPort: "value"},
		},
	}
}

// TestPartition_OnEvent_DataEdgeIsNotExecTarget is the ADR 013 finale
// regression: an on-event whose sole out-edge is data must produce an
// entry with an EMPTY exec Target (not the data node). Otherwise the
// runtime exec interpreter looks the data node up in the exec node table,
// misses, and logs "exec: unknown node id".
func TestPartition_OnEvent_DataEdgeIsNotExecTarget(t *testing.T) {
	m := execManifest()
	m["core.data.get-field@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	m["core.output@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": reactiveOnEventBlueprint()},
		manifest:   m,
	}
	g, _, _, err := Compile(context.Background(), "scene-r",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatalf("reactive on-event scene rejected: %v", err)
	}
	if len(g.ExecPrograms) != 1 {
		t.Fatalf("want 1 exec program, got %d", len(g.ExecPrograms))
	}
	p := decodeProgram(t, g.ExecPrograms[0])
	e, ok := p.Entrypoints["onChat"]
	if !ok {
		t.Fatalf("on-event entry missing; entries = %+v", p.Entrypoints)
	}
	if e.Kind != "on-event" || e.Event != "stream_chat_event" {
		t.Fatalf("entry = %+v, want kind on-event event stream_chat_event", e)
	}
	if e.Target.Node != "" {
		t.Fatalf("entry Target = %+v, want EMPTY (data-only out-edge must not become an exec target)", e.Target)
	}
	// The exec program carries no body nodes (the chain is pure dataflow).
	if len(p.Nodes) != 0 {
		t.Fatalf("exec program nodes = %+v, want none (dataflow-only reactive scene)", p.Nodes)
	}
	// The data nodes survive in the data graph and stay reactive.
	if _, ok := graphNodeByID(g, "text"); !ok {
		t.Fatal("get-field data node dropped from the data graph")
	}
	if _, ok := graphNodeByID(g, "out"); !ok {
		t.Fatal("output sink dropped from the data graph")
	}
}

// TestPartition_OnCall_KeyedByConfigEntrypoint proves the operator-call
// contract fix: a `core.operator.on-call@1` entry is keyed in the program's
// Entrypoints map (the {entrypoint_id} the cockpit addresses) by its
// `config.entrypoint`, decoupled from the graph node id — with a fallback to
// the node id when the config is absent (retro-compat).
func TestPartition_OnCall_KeyedByConfigEntrypoint(t *testing.T) {
	m := execManifest()
	m["core.operator.on-call@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}

	compile := func(bpID, entrypoint string) execProgram {
		arm := BlueprintNode{
			ID: "on_call_arm", Compute: "core.operator.on-call@1",
			Outputs: []BlueprintPort{execOut("then"), dataOut("payload")},
		}
		if entrypoint != "" {
			arm.Config = map[string]json.RawMessage{"entrypoint": json.RawMessage(`"` + entrypoint + `"`)}
		}
		bp := &BlueprintGraph{ID: bpID, Nodes: []BlueprintNode{arm}}
		f := &fakeFetcher{
			layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
			blueprints: map[string]*BlueprintGraph{bpID: bp},
			manifest:   m,
		}
		g, _, _, err := Compile(context.Background(), "scene-oncall",
			PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: bpID}, f)
		if err != nil {
			t.Fatalf("on-call scene rejected: %v", err)
		}
		if len(g.ExecPrograms) != 1 {
			t.Fatalf("want 1 exec program, got %d", len(g.ExecPrograms))
		}
		return decodeProgram(t, g.ExecPrograms[0])
	}

	// Named: keyed by config.entrypoint, NOT the node id.
	named := compile("bp-named", "marker_overlay_on")
	e, ok := named.Entrypoints["marker_overlay_on"]
	if !ok {
		t.Fatalf("on-call not keyed by config.entrypoint; entries = %+v", named.Entrypoints)
	}
	if _, wrong := named.Entrypoints["on_call_arm"]; wrong {
		t.Fatal("on-call wrongly keyed by node id when config.entrypoint present")
	}
	if e.Kind != "on-call" || e.Node != "on_call_arm" {
		t.Fatalf("entry = %+v, want kind on-call node on_call_arm (payload binds under the node id)", e)
	}

	// Fallback: no config.entrypoint → keyed by node id (retro-compat).
	fb := compile("bp-fallback", "")
	if _, ok := fb.Entrypoints["on_call_arm"]; !ok {
		t.Fatalf("on-call fallback not keyed by node id; entries = %+v", fb.Entrypoints)
	}
}

func TestPartition_MixedScene_Structure(t *testing.T) {
	g := compileMixed(t)

	for _, id := range []string{"br", "set", "start"} {
		if _, ok := graphNodeByID(g, id); ok {
			t.Fatalf("exec node %q leaked into the data GraphNode list", id)
		}
	}
	// `val` and `cond` literals feed exec data pins but are themselves
	// pure data leaves → they stay data nodes (seed Defaults).
	if _, ok := graphNodeByID(g, "val"); !ok {
		t.Fatal("pure literal val dropped from the data graph")
	}

	if len(g.ExecPrograms) != 1 {
		t.Fatalf("want 1 exec program, got %d", len(g.ExecPrograms))
	}
	p := decodeProgram(t, g.ExecPrograms[0])

	if p.BlueprintKey != "" {
		t.Fatalf("legacy blueprint key should be empty, got %q", p.BlueprintKey)
	}
	// Entry: on-start, Target = branch.
	e, ok := p.Entrypoints["start"]
	if !ok {
		t.Fatalf("on-start entry missing; entries = %+v", p.Entrypoints)
	}
	if e.Kind != "on-start" || e.Target.Node != "br" {
		t.Fatalf("entry = %+v, want kind on-start, target br", e)
	}
	// branch node: op branch, Next["true"] → set.
	br, ok := p.Nodes["br"]
	if !ok || br.Op != "branch" {
		t.Fatalf("branch node = %+v, want op branch", br)
	}
	if tgt, ok := br.Next["true"]; !ok || tgt.Node != "set" {
		t.Fatalf("branch Next[true] = %+v, want set", br.Next)
	}
	// branch condition: data edge into the exec node → ExecDataInput.
	if len(br.Data) != 1 || br.Data[0].Port != "condition" || br.Data[0].From != "cond" {
		t.Fatalf("branch data = %+v, want one {condition,cond}", br.Data)
	}
	// set node: op variable.set, config carried verbatim, value pulled.
	set, ok := p.Nodes["set"]
	if !ok || set.Op != "variable.set" {
		t.Fatalf("set node = %+v, want op variable.set", set)
	}
	if string(set.Config["name"]) != `"counter"` {
		t.Fatalf("set config.name = %s, want \"counter\" (verbatim)", set.Config["name"])
	}
	if len(set.Data) != 1 || set.Data[0].Port != "value" || set.Data[0].From != "val" {
		t.Fatalf("set data = %+v, want one {value,val}", set.Data)
	}
}

// Criterion #2 (golden): a pure-dataflow scene is byte-identical before
// and after the lift — ExecPrograms stays nil (omitempty), the rest of
// the graph and the scene_version hash are untouched.
func TestPartition_PureDataflow_ByteIdentical(t *testing.T) {
	// scoreBlueprint is a pure data blueprint (no exec pin).
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": scoreBlueprint("bp-1", "value")},
		manifest:   pureManifest(),
	}
	g, _, version, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	if err != nil {
		t.Fatalf("pure scene rejected: %v", err)
	}
	if g.ExecPrograms != nil {
		t.Fatalf("pure-dataflow scene carries exec programs: %v", g.ExecPrograms)
	}
	// The serialized graph must NOT contain an exec_programs key
	// (omitempty) — that is the byte-identity guarantee.
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatal(err)
	}
	if _, present := asMap["exec_programs"]; present {
		t.Fatalf("pure scene graph serialized an exec_programs key — not byte-identical")
	}
	// GOLDEN: the scene_version hash is pinned. If a future change to the
	// partition perturbs a pure scene, this constant changes and the
	// byte-identity invariant is broken loudly.
	if version != pureScoreSceneVersion {
		t.Fatalf("pure scene_version drifted: got %s, golden %s\n"+
			"(if intentional, the partition changed a pure scene — ADR 006 §3.1 forbids it)",
			version, pureScoreSceneVersion)
	}
}

// pureScoreSceneVersion is the byte-identity golden for the pure-dataflow
// score scene (ADR 006 §3.1: a scene with no exec pin must hash
// identically to pre-lift). Captured from a clean compile; it must NOT
// change when the exec partition lands.
const pureScoreSceneVersion = "sha256:3ef45864bc022fd30a4bda5a9740b95f98fc607de6a413e9f0389c8aba52a4c0"

// Criterion #2 (determinism): the same envelope compiled twice yields a
// byte-identical artefact, including exec_programs order and the
// per-program map serialization. Compiled N times to rule out
// map-iteration order leaking in.
func TestPartition_Deterministic(t *testing.T) {
	var first []byte
	for i := 0; i < 8; i++ {
		g := compileMixed(t)
		raw, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = raw
			continue
		}
		if string(raw) != string(first) {
			t.Fatalf("compile %d diverged from compile 0:\n got %s\nwant %s", i, raw, first)
		}
	}
}

// Keyed blueprint: a data-producer feeding an exec data pin is addressed
// by its MERGED (key-prefixed) id in ExecDataInput.From — matching the
// prefixed data-node id the runtime's demandValue resolves against. The
// exec node id, Next targets and entry targets stay UNPREFIXED (resolved
// within the program). This pins the cross-layer namespacing convention
// #105/#106 depend on.
func TestPartition_KeyedBlueprint_DataFromPrefixed(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "lit", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`5`)},
				Outputs: []BlueprintPort{dataIn("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"c"`)},
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "lit", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		manifest:   execManifest(),
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", Blueprints: []BlueprintRef{{Key: "score", ID: "bp-1"}}}, f)
	if err != nil {
		t.Fatalf("keyed exec scene rejected: %v", err)
	}
	// The data producer is merged under the key prefix.
	if _, ok := graphNodeByID(g, "score.lit"); !ok {
		t.Fatalf("keyed data producer missing; nodes = %+v", g.Nodes)
	}
	p := decodeProgram(t, g.ExecPrograms[0])
	if p.BlueprintKey != "score" {
		t.Fatalf("program key = %q, want score", p.BlueprintKey)
	}
	// Exec node id stays UNPREFIXED inside the program.
	set, ok := p.Nodes["set"]
	if !ok {
		t.Fatalf("set node missing (should be unprefixed); nodes = %+v", p.Nodes)
	}
	// But its data From points at the PREFIXED data producer id.
	if len(set.Data) != 1 || set.Data[0].From != "score.lit" {
		t.Fatalf("ExecDataInput.From = %+v, want score.lit (prefixed)", set.Data)
	}
	// Entry target is the unprefixed exec node id.
	if e := p.Entrypoints["start"]; e.Target.Node != "set" {
		t.Fatalf("entry target = %q, want unprefixed set", e.Target.Node)
	}
}

// EXEC_OP_UNMAPPED (fail-loud): a node carrying an exec pin whose
// manifest id maps to no runtime exec op is rejected structurally — a
// coverage gap can never silently become accept-then-ignore. This is
// NOT a capability rejection (a conformant build never hits it).
// danglingOnStartBlueprint reproduces the issue #100 leak post-expansion: an
// on-start whose EXEC out pin (`then`) is wired to a DATA node's data pin. This
// is exactly the shape a PURE blueprint reference produces when its body still
// carried a core.event.on-start@1: #186 preserves the on-start for a data-only
// ref, then inlining splices the on-start's `then` across the dropped
// core.output@1 onto the parent's data consumer. The on-start becomes an
// entrypoint whose Target lands on a data node (`guard`, a not-equal compute),
// which is NOT in the exec node table → at arming the runtime would log
// "exec: unknown node id" and silently drop the chain. The compiler must
// instead reject the push with EXEC_UNKNOWN_NODE.
func danglingOnStartBlueprint() *BlueprintGraph {
	return &BlueprintGraph{
		ID: "bp-dangling",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []BlueprintPort{execOut("then")}},
			// A pure DATA node (no exec pin) — the spliced on-start `then`
			// lands on its data input `a`, the dangling exec target.
			{ID: "guard", Compute: "core.compare.not-equal@1",
				Inputs:  []BlueprintPort{dataIn("a"), dataIn("b")},
				Outputs: []BlueprintPort{dataIn("result")}},
			{ID: "out", Compute: "core.output@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"x"`)},
				Inputs: []BlueprintPort{dataIn("value")}},
		},
		Edges: []BlueprintEdge{
			// the dangling exec edge: on-start.then (exec) → guard.a (data)
			{FromNode: "start", FromPort: "then", ToNode: "guard", ToPort: "a"},
			{FromNode: "guard", FromPort: "result", ToNode: "out", ToPort: "value"},
		},
	}
}

// TestPartition_DanglingExecTarget_FailsLoud (issue #100): an exec entrypoint
// whose Target resolves to a non-exec node is rejected with EXEC_UNKNOWN_NODE
// — the compile-time mirror of the runtime's "unknown node id" miss. Without
// this gate the push reported `validated` while logging ERRORs and arming a
// silently-broken scene (the "false clean").
func TestPartition_DanglingExecTarget_FailsLoud(t *testing.T) {
	m := execManifest()
	m["core.compare.not-equal@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	m["core.output@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": danglingOnStartBlueprint()},
		manifest:   m,
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrExecUnknownNode) {
		t.Fatalf("want EXEC_UNKNOWN_NODE, got %v", err)
	}
}

// TestValidateExecTargets_DanglingNext: a Next edge pointing at a node absent
// from the exec table is rejected (the node-level counterpart of the entry
// case). Built directly on the program mirror so the path is unit-covered.
func TestValidateExecTargets_DanglingNext(t *testing.T) {
	p := &execProgram{
		BlueprintKey: "",
		Nodes: map[string]*execNode{
			"a": {ID: "a", Op: "variable.set", Next: map[string]execTarget{
				"then": {Node: "ghost", Port: "exec_in"}, // ghost not in Nodes
			}},
		},
		Entrypoints: map[string]execEntry{},
	}
	diags := validateExecTargets(p)
	if len(diags) != 1 || diags[0].Code != ErrExecUnknownNode {
		t.Fatalf("want one EXEC_UNKNOWN_NODE, got %+v", diags)
	}
	if diags[0].Path != "a" {
		t.Fatalf("diag Path = %q, want offending node id \"a\"", diags[0].Path)
	}
}

// TestValidateExecTargets_HealthyProgram: a well-formed program (every Target /
// Next resolves; an empty no-op entry Target is allowed) yields NO diagnostics
// — the anti-regression guard for a sane graph (criterion #2 byte-identical).
func TestValidateExecTargets_HealthyProgram(t *testing.T) {
	p := &execProgram{
		Nodes: map[string]*execNode{
			"set": {ID: "set", Op: "variable.set", Next: map[string]execTarget{
				"then": {Node: "set2", Port: "exec_in"},
			}},
			"set2": {ID: "set2", Op: "variable.set"},
		},
		Entrypoints: map[string]execEntry{
			"start": {Kind: "on-start", Node: "start", Target: execTarget{Node: "set", Port: "exec_in"}},
			"noop":  {Kind: "on-start", Node: "noop"}, // empty Target — valid no-op
		},
	}
	if diags := validateExecTargets(p); len(diags) != 0 {
		t.Fatalf("healthy program produced diagnostics: %+v", diags)
	}
}

func TestPartition_ExecOpUnmapped_FailsLoud(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-1",
		Nodes: []BlueprintNode{
			{ID: "weird", Compute: "core.flow.teleport@1",
				Inputs:  []BlueprintPort{execIn("exec_in")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
	}
	m := execManifest()
	// Manifest-known (so it passes the unknown-compute gate) but mapped
	// to no runtime op in the conformance table.
	m["core.flow.teleport@1"] = ComputeManifestEntry{IsPure: true, Version: "1"}

	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-1": bp},
		manifest:   m,
	}
	_, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-1"}, f)
	var ce *CompileError
	if !errors.As(err, &ce) || !ce.HasCode(ErrExecOpUnmapped) {
		t.Fatalf("want EXEC_OP_UNMAPPED, got %v", err)
	}
}
