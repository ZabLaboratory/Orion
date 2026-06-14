package compiler

import (
	"context"
	"strings"
	"testing"
)

// These are Forge's proximity tests for Orion #186: exec-pin mapping during
// blueprint-reference expansion. An exec-triggerable referenced function
// (interface declares a `kind:exec` input pin) has its internal
// `core.event.on-start@1` REMOVED and its spine re-armed by the caller's
// spine through the `exec_in` splice; a data-only reference is untouched.
// The exhaustive matrix is Probe (#180); these cover the two
// graph-resolution.md § Exec pins clauses + the anti-regression guard-rail.

// execRefManifest is the manifest the exec-reference tests need: the pure
// data computes plus the event entry and the db.query effect the referenced
// functions carry.
func execRefManifest() ComputeManifest {
	m := pureManifest()
	m[coreEventOnStart] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	m["core.db.query@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	m["core.output@1"] = ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}
	return m
}

// execTriggerableFetch is a referenced function authored exec-triggerable:
// its interface declares an exec INPUT pin `exec_in` and an exec OUTPUT pin
// `then`. Internally it still carries a `core.event.on-start@1` whose spine
// drives a `core.db.query@1` — exactly the shape Orion #186 must neutralise
// (the on-start must NOT survive into the parent; the caller's spine, spliced
// onto `exec_in`, becomes the trigger).
//
//	exec_in (core.input@1, kind:exec) --then--> q (db.query)
//	start   (on-start)                --then--> q (db.query)
func execTriggerableFetch(id string, version int) *ResolvedBlueprintGraph {
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			// exec entry interface node: config.name == "exec_in".
			inputNode("execEntry", "exec_in"),
			{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
			{ID: "q", Compute: "core.db.query@1",
				Inputs:  []BlueprintPort{execIn("exec_in")},
				Outputs: []BlueprintPort{execOut("then")}},
		},
		Edges: []BlueprintEdge{
			{FromNode: "execEntry", FromPort: "then", ToNode: "q", ToPort: "exec_in"},
			{FromNode: "start", FromPort: "then", ToNode: "q", ToPort: "exec_in"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "exec_in", Type: "exec", Kind: "exec", Required: true}},
			Outputs: []BlueprintInterfacePin{{Name: "then", Type: "exec", Kind: "exec"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

// compileExecRefScene compiles a caller scene that references one function.
func compileExecRefScene(t *testing.T, bp *BlueprintGraph, graphs map[string]*ResolvedBlueprintGraph) *Graph {
	t.Helper()
	f := &fakeFetcher{
		layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
		blueprints: map[string]*BlueprintGraph{"bp-scene": bp},
		components: map[ComponentRef]*UserComponent{},
		manifest:   execRefManifest(),
		graphs:     graphs,
	}
	g, _, _, err := Compile(context.Background(), "scene-1",
		PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return g
}

// (a) An exec-triggerable reference: the caller's on-start spine arms the
// `exec_in` of the function; expansion must produce ZERO inlined on-start
// (only the caller's parent on-start survives) and the caller's spine must
// reach the inlined `core.db.query@1` (it partitioned to EXEC).
func TestExpand_ExecTriggerableReference_DropsInlinedOnStart(t *testing.T) {
	// caller: start --then--> fetch(exec_in). The reference's `then` output
	// is left unwired (a terminal effect) — the spine ends at the db.query.
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
			refNode("fetch", "bp-fetch", 1),
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "fetch", ToPort: "exec_in"},
		},
	}
	g := compileExecRefScene(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-fetch@1": execTriggerableFetch("bp-fetch", 1),
	})

	// No `reference` node leaked.
	for _, n := range g.Nodes {
		if n.Compute == "blueprint.reference" {
			t.Fatalf("reference node %s leaked into the runtime graph", n.ID)
		}
	}

	// Exactly one exec program; it carries exactly ONE on-start entry — the
	// caller's `start`. The inlined function's on-start must be gone (it
	// would otherwise re-arm the body at scene load, ADR 003 §1.1).
	if len(g.ExecPrograms) != 1 {
		t.Fatalf("want 1 exec program, got %d", len(g.ExecPrograms))
	}
	p := decodeProgram(t, g.ExecPrograms[0])

	onStartEntries := make([]string, 0, len(p.Entrypoints))
	for id, e := range p.Entrypoints {
		if e.Kind == "on-start" {
			onStartEntries = append(onStartEntries, id)
		}
	}
	if len(onStartEntries) != 1 {
		t.Fatalf("want exactly 1 on-start entry (the caller's), got %d: %v\nentries=%+v",
			len(onStartEntries), onStartEntries, p.Entrypoints)
	}
	if onStartEntries[0] != "start" {
		t.Fatalf("surviving on-start entry = %q, want the caller's \"start\" (inlined on-start should be dropped)", onStartEntries[0])
	}

	// The caller's spine reaches the inlined db.query (alpha-renamed). Its
	// node id is prefixed __bpref…; it is the entry's Target.
	e := p.Entrypoints["start"]
	if e.Target.Node == "" {
		t.Fatalf("caller on-start entry has empty Target — spine did not splice onto the inlined exec body")
	}
	if !strings.Contains(e.Target.Node, "__bpref") {
		t.Fatalf("entry Target %q is not an inlined (alpha-renamed) node", e.Target.Node)
	}
	qNode, ok := p.Nodes[e.Target.Node]
	if !ok {
		t.Fatalf("entry Target %q absent from exec program nodes %+v", e.Target.Node, p.Nodes)
	}
	if qNode.Op != "db.query" {
		t.Fatalf("caller spine arms %q (op %q), want the inlined core.db.query@1", e.Target.Node, qNode.Op)
	}
}

// dataOnlyFetch is a referenced function with NO exec pin: it declares only
// data interface pins, yet internally carries an `on-start` → db.query spine
// (a self-driving function). Orion #186 must NOT touch its on-start — that
// would silently break every pre-#186 data-only reference (Vigil's
// anti-regression guard-rail).
//
//	start (on-start) --then--> q (db.query); out (core.output@1 "result").
func dataOnlyFetch(id string, version int) *ResolvedBlueprintGraph {
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			inputNode("in", "x"),
			{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
			{ID: "q", Compute: "core.db.query@1",
				Inputs: []BlueprintPort{execIn("exec_in")}},
			outputNode("out", "result"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "q", ToPort: "exec_in"},
		},
		Interface: BlueprintInterface{
			// Pure data interface — Kind empty/absent → defaults to data.
			Inputs:  []BlueprintInterfacePin{{Name: "x", Type: "float"}},
			Outputs: []BlueprintInterfacePin{{Name: "result", Type: "float"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

// (b) A data-only reference: the inlined `on-start` MUST be preserved. This
// is the non-regression guard — a function that declares no exec input pin
// behaves exactly as before #186 (its self-driving spine still fires at
// scene load).
func TestExpand_DataOnlyReference_PreservesInlinedOnStart(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			inputNode("seed", "score.seed"),
			refNode("fn", "bp-data", 2),
			outputNode("sink", "score.final"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "seed", FromPort: "value", ToNode: "fn", ToPort: "x"},
			{FromNode: "fn", FromPort: "result", ToNode: "sink", ToPort: "value"},
		},
	}
	g := compileExecRefScene(t, bp, map[string]*ResolvedBlueprintGraph{
		"bp-data@2": dataOnlyFetch("bp-data", 2),
	})

	if len(g.ExecPrograms) != 1 {
		t.Fatalf("want 1 exec program (the inlined on-start spine), got %d", len(g.ExecPrograms))
	}
	p := decodeProgram(t, g.ExecPrograms[0])

	// The inlined on-start is PRESERVED: there is exactly one on-start entry,
	// and it is the alpha-renamed inlined node (not dropped).
	onStartEntries := make([]string, 0, len(p.Entrypoints))
	for id, e := range p.Entrypoints {
		if e.Kind == "on-start" {
			onStartEntries = append(onStartEntries, id)
		}
	}
	if len(onStartEntries) != 1 {
		t.Fatalf("data-only reference: want its inlined on-start PRESERVED (1 entry), got %d: %v",
			len(onStartEntries), onStartEntries)
	}
	if !strings.Contains(onStartEntries[0], "__bpref") {
		t.Fatalf("preserved on-start %q is not the inlined (alpha-renamed) node", onStartEntries[0])
	}
	// It still arms the inlined db.query.
	e := p.Entrypoints[onStartEntries[0]]
	qNode, ok := p.Nodes[e.Target.Node]
	if !ok || qNode.Op != "db.query" {
		t.Fatalf("preserved on-start Target %+v does not arm the inlined db.query (node %+v)", e.Target, qNode)
	}
}

// (c) Determinism: the same exec-triggerable reference compiles to the same
// scene_version hash across runs (the alpha-rename / on-start drop is stable).
func TestExpand_ExecTriggerableReference_Deterministic(t *testing.T) {
	mk := func() *BlueprintGraph {
		return &BlueprintGraph{
			ID: "bp-scene",
			Nodes: []BlueprintNode{
				{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
				refNode("fetch", "bp-fetch", 1),
			},
			Edges: []BlueprintEdge{
				{FromNode: "start", FromPort: "then", ToNode: "fetch", ToPort: "exec_in"},
			},
		}
	}
	hashOnce := func() string {
		f := &fakeFetcher{
			layouts:    map[string]*CanvasLayout{"v1": minimalLayout("v1")},
			blueprints: map[string]*BlueprintGraph{"bp-scene": mk()},
			components: map[ComponentRef]*UserComponent{},
			manifest:   execRefManifest(),
			graphs:     map[string]*ResolvedBlueprintGraph{"bp-fetch@1": execTriggerableFetch("bp-fetch", 1)},
		}
		_, _, version, err := Compile(context.Background(), "scene-1",
			PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, f)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return version
	}
	a, b := hashOnce(), hashOnce()
	if a != b {
		t.Fatalf("non-deterministic exec-reference expansion: %q != %q", a, b)
	}
}
