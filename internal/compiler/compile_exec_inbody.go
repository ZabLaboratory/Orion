package compiler

import "encoding/json"

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

// CompiledExecInBody is the result of an in-body exec compile: the marshalled
// exec programs (the bytes graph.ExecPrograms carries) plus the constant
// `__vars..` seeds harvested from the graph's variables[]. The caller folds
// both onto a minimal compiler.Graph the harness then drives.
type CompiledExecInBody struct {
	// Programs is one marshalled runtime.ExecProgram per exec-bearing
	// blueprint — here always 0 or 1 (a single draft blueprint per request,
	// legacy key "", ADR 015 §A1.2). Empty when the graph carries no exec
	// node (a pure-dataflow draft: no exec spine to fire).
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

	// Fold this blueprint's `variables[].value` constants into the seeds the
	// inlined `core.variable.get@1` reads (Orion #192). On the push path the
	// reference expander harvests these into BlueprintGraph.Defaults; in-body
	// there is no expander, so we harvest them here directly from the
	// authoring graph. The empty-key `__vars..<name>` form (varsLeaf) is
	// key-namespaced by prefixDefaultLeaf, byte-identical to the leaf
	// prefixGraphNodes wrote on the reading node and to execVariableSet's
	// write address. A value-less variable (pure shared state) is skipped.
	for _, v := range bp.Variables {
		if len(v.Value) == 0 || v.Name == "" {
			continue
		}
		out.Defaults[prefixDefaultLeaf(key, varsLeaf(v.Name))] = v.Value
	}

	return out, nil
}
