package compiler

import (
	"encoding/json"

	"github.com/ZabLaboratory/Orion/internal/conformance"
)

// This file is the in-body exec-compile seam (ADR 015 Amendment 1 §A1.3):
// the validate/simulate endpoint must compile an authoring-level Blue graph
// into the exec programs the validation harness fires — WITHOUT the network
// compile path. `Compile` (compile.go) needs a Fetcher to pull the Canvas
// layout, the per-blueprint graphs and Blue's compute manifest; that egress
// is exactly the surface the simulate endpoint refuses (A1.4: zero egress is
// the invariant that keeps the #198 Bastion clearance valid). The exec
// partition, by contrast, needs ONLY the in-body graph — it reads
// node.definition, the input/output port Kind discriminators, the edges and
// the variables. So this seam runs the EXISTING partition (partitionBlueprint
// + validateExecTargets + marshalExecProgram, exec_partition.go) directly on
// the supplied BlueprintGraph and never touches a Fetcher, a manifest, the
// store, or the data tranche (the render layer is not exercised in simulate).
//
// What it deliberately does NOT do, vs Compile:
//   - no manifest fetch / no manifest validation of data nodes (no Fetcher);
//   - no reference expansion (expandReferences needs a Fetcher to resolve a
//     pinned published version — that resolve is egress). A `reference` node
//     is rejected up front (REFERENCE_NOT_SUPPORTED, §A1.3 c), keeping the
//     no-egress invariant intact;
//   - no data-node compile / no topo-sort / no render lowering / no
//     scene_version hash. The harness clones a Graph whose data tranche is
//     empty — simulate proves the exec spine, not the rendered bundle.

// ErrReferenceNotSupported rejects a `reference` node on the in-body compile
// path (ADR 015 §A1.3 c). The ADR 014 expansion of a blueprint reference
// resolves a pinned published version over HTTP (a Fetcher) — the exact
// egress the simulate endpoint refuses. Rather than silently dropping the
// node (the class of muteness this amendment closes), the seam fails loud
// under COMPILE_FAILED so the caller learns the reference was descoped, not
// that the graph produced no entrypoints.
const ErrReferenceNotSupported DiagnosticCode = "REFERENCE_NOT_SUPPORTED"

// ErrUnknownNode rejects a node whose `definition` is not in Orion's served
// set (ADR 015 §A1.3 step 3: "noeud `definition` inconnu / non servi →
// diagnostic compilateur"). The exec partition's EXEC_OP_UNMAPPED only fires
// for an unknown node that ALSO carries an exec pin (so it is routed to the
// exec layer); an unknown node with no exec pin — the live-observed
// `core.nonexistent.fake-node@99` left UNWIRED — would otherwise fall through
// as a "data node" the in-body seam never compiles, and the request returned
// a silent `blueprints: null` (the muteness this amendment closes). This pass
// validates EVERY node id against conformance.Classify (the single served-set
// source of truth, CI-cross-checked) up front, so an unknown definition fails
// loud under COMPILE_FAILED whether or not it sits in an exec spine.
const ErrUnknownNode DiagnosticCode = "UNKNOWN_NODE"

// ErrNoExecProgram rejects an in-body simulate graph that compiles cleanly but
// produces ZERO exec programs (ADR 015 §A1.3 step 4 expects a non-null
// `blueprints`). Simulate is a dry-run that FIRES entrypoints against a
// synthetic event; a graph with no exec node — no on-start/on-tick/on-event
// spine — has nothing to fire, so returning a silent 200 `blueprints: null`
// hides an authoring mistake (the residue the parent issue reports). Unlike
// the push path (where a pure-dataflow scene trivially validates — Harness
// doc), submitting such a graph TO SIMULATE is almost certainly an author
// error: there is no executable program to exercise. We fail loud so the
// bluemcp agent learns "no entrypoint" instead of an empty report.
const ErrNoExecProgram DiagnosticCode = "NO_EXEC_PROGRAM"

// CompiledExecInBody is the result of an in-body exec compile: the marshalled
// exec programs (the bytes graph.ExecPrograms carries) plus the constant
// `__vars..` seeds harvested from the graph's variables[]. The caller folds
// both onto a minimal compiler.Graph the harness then drives.
type CompiledExecInBody struct {
	// Programs is one marshalled runtime.ExecProgram per exec-bearing
	// blueprint — here always exactly 1 (a single draft blueprint per request,
	// legacy key "", ADR 015 §A1.2). CompileExecPrograms never returns a
	// CompiledExecInBody with zero programs: a graph with no exec spine is
	// rejected as NO_EXEC_PROGRAM (nothing to simulate), so a successful
	// compile always carries at least one program.
	Programs []json.RawMessage
	// Defaults are the `__vars.<key>.<name>` constant seeds the exec
	// interpreter's variable.get reads (Orion #192 mechanism), key-prefixed
	// the same way prefixDefaultLeaf namespaces a push-compiled default.
	Defaults map[string]json.RawMessage
}

// CompileExecPrograms compiles the exec layer of an authoring-level Blue
// graph in-body, with NO Fetcher and NO network egress (ADR 015 §A1.3). It
// runs the existing exec partition on `bp` under the scene-local blueprint
// `key` (the legacy single-key "" in MVP) and returns the marshalled exec
// programs plus the variable seeds. A `reference` node is rejected
// (REFERENCE_NOT_SUPPORTED); any partition error-diagnostic (unknown exec op,
// dangling exec target, …) is returned as a *CompileError — the caller maps
// it to 400 COMPILE_FAILED, distinct from INVALID_GRAPH (a malformed body).
func CompileExecPrograms(bp *BlueprintGraph, key string) (*CompiledExecInBody, *CompileError) {
	d := &Diagnostics{}

	// A `reference` node would require the ADR 014 expansion — a Fetcher
	// resolve of a pinned published version, i.e. egress. Reject it loudly
	// (descoped MVP, §A1.3 c) before the partition, so a reference never
	// silently drops to "no exec program" (the muteness this closes).
	for _, n := range bp.Nodes {
		if n.Reference != nil {
			d.AddErrorAt(ErrReferenceNotSupported, n.ID,
				"node %s is a blueprint `reference` — in-body simulate does not "+
					"resolve references (ADR 014 expansion requires upstream resolution; "+
					"descoped, ADR 015 Amendment 1)", n.ID)
		}
	}
	if d.HasErrors() {
		return nil, &CompileError{Diagnostics: *d}
	}

	// Validate EVERY node's `definition` against the served set up front, so an
	// unknown node is rejected whether or not it carries an exec pin (§A1.3
	// step 3). The exec partition's EXEC_OP_UNMAPPED only catches an unknown
	// node routed to the exec layer (one with an exec pin); an unknown node
	// left as a "data node" (no exec pin, unwired — the live `fake-node@99`
	// case) would otherwise slip through silently, because the in-body seam
	// compiles only the exec tranche. conformance.Classify is the single
	// served-set source of truth (CI-cross-checked); ok=false ⇒ unknown.
	for _, n := range bp.Nodes {
		if _, ok := conformance.Classify(n.Compute); !ok {
			d.AddErrorAt(ErrUnknownNode, n.ID,
				"node %s definition %q is not a served Orion node", n.ID, n.Compute)
		}
	}
	if d.HasErrors() {
		return nil, &CompileError{Diagnostics: *d}
	}

	// Run the EXACT push-path partition (exec_partition.go) — it consumes
	// only the in-body graph. partitionBlueprint already appends
	// validateExecTargets' dangling-target diagnostics.
	_, prog, partDiags := partitionBlueprint(bp, key)
	d.Items = append(d.Items, partDiags...)
	if d.HasErrors() {
		return nil, &CompileError{Diagnostics: *d}
	}

	out := &CompiledExecInBody{Defaults: map[string]json.RawMessage{}}

	if prog != nil {
		raw, err := marshalExecProgram(prog)
		if err != nil {
			d.AddError(ErrTopologySort, "exec program marshal: %v", err)
			return nil, &CompileError{Diagnostics: *d}
		}
		out.Programs = append(out.Programs, raw)
	}

	// Zero exec programs means the graph carries no exec spine — no
	// on-start/on-tick/on-event entrypoint to fire. For simulate (a dry-run
	// AGAINST a synthetic event) there is nothing to exercise, so a silent 200
	// `blueprints: null` would hide an authoring mistake. Fail loud with
	// NO_EXEC_PROGRAM so the caller learns "no entrypoint" rather than reading
	// an empty report (the residue this change closes). Note this is the
	// in-body simulate contract; the push path's pure-dataflow scene still
	// validates trivially (Harness.Validate doc) — that path never reaches here.
	if len(out.Programs) == 0 {
		d.AddError(ErrNoExecProgram,
			"graph produced no executable program — no recognised entrypoint "+
				"(on-start/on-tick/on-event) is wired into an exec spine; nothing to simulate")
		return nil, &CompileError{Diagnostics: *d}
	}

	// Fold this blueprint's `variables[].value` constants into the seeds the
	// inlined `core.variable.get@1` reads (Orion #192). On the push path the
	// reference expander harvests INLINED references' constants into
	// BlueprintGraph.Defaults, and the compile loop now also folds a top-level
	// blueprint's OWN declared variables via the SAME foldDeclaredVariables
	// helper (ADR 016 RC-6); in-body there is no expander, so we harvest the
	// authoring graph's variables here with that shared helper — guaranteeing
	// the two paths never drift. foldDeclaredVariables emits the empty-key
	// `__vars..<name>` form; prefixDefaultLeaf then key-namespaces it,
	// byte-identical to the leaf prefixGraphNodes wrote on the reading node and
	// to execVariableSet's write address. A value-less variable (pure shared
	// state) is skipped by the helper.
	declared := map[string]json.RawMessage{}
	foldDeclaredVariables(declared, bp.Variables)
	for leaf, v := range declared {
		out.Defaults[prefixDefaultLeaf(key, leaf)] = v
	}

	return out, nil
}
