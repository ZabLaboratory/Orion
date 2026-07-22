package compiler

import (
	"context"
	"encoding/json"
)

// CompileBlueprintRule compiles a SINGLE Blue blueprint into a COMPLETE runtime
// Graph for a blueprint-direct stream rule (ADR 017). It is the counterpart of
// CompileExecPrograms (the ADR 015 in-body simulate seam), which is exec-only
// and zero-egress by contract: that seam clones a Graph whose data tranche is
// EMPTY, so a promoted blueprint-direct rule carried no Nodes → nodeIdx was
// empty at runtime → any exec input fed by a data-plane edge (e.g. `app_id` on
// `core.overlay-app.set@1`) resolved to nothing, silently. This entrypoint
// closes that class by materialising the FULL data tranche (Nodes, Bindings,
// Defaults) alongside the exec programs, reusing the EXACT scene-path helpers
// (partitionBlueprint, validateBlueprint, foldDeclaredVariables,
// topologicalSort, the platform/event acceptance bindings) — so a rule compiled
// here is equivalent in substance to the same blueprint compiled through the
// scene path (ADR 017 §6 RC-2, parity modulo the key prefix).
//
// The compute manifest is fetched through the SAME Fetcher the promote path
// already holds (the caller has just called FetchBlueprint on it): no interface
// extension, no new egress surface beyond the one manifest handshake the scene
// path already makes. The seam simulate machinery (CompileExecPrograms) is left
// byte-identical — the two consumers now each sit on the entrypoint that carries
// their contract (ADR 017 §3.3).
//
// `key` namespaces the blueprint's node ids / leaves / `__vars` exactly as the
// scene loop's per-blueprint key does (prefixGraphNodes / prefixDefaultLeaf).
// The promote/reload callers pass the blueprint_id (preserving today's
// operator-call address); parity tests compile both paths under the same key so
// the data tranches compare byte-identically.
//
// A blueprint with NO exec spine is rejected fail-loud (ErrNoExecProgram): a
// stream rule with no entrypoint (on-start/on-tick/on-event) has nothing to run
// — the data-tranche analogue of the simulate seam's NO_EXEC_PROGRAM (ADR 017
// §6 RC-7 / §3.1). A pure-dataflow blueprint is a valid SCENE (it validates
// trivially on the push path) but not a valid RULE.
func CompileBlueprintRule(ctx context.Context, bp *BlueprintGraph, key string, fetcher Fetcher) (*Graph, *CompileError) {
	d := &Diagnostics{}

	manifest, err := fetcher.FetchComputeManifest(ctx)
	if err != nil {
		d.AddError(ErrFetchUpstream, "fetch compute manifest: %v", err)
		return nil, &CompileError{Diagnostics: *d}
	}

	// Partition first: the exec node set drives BOTH the data-node exclusion in
	// validateBlueprint and the ExecProgram emission — same ordering as the
	// scene loop (compile.go). partitionBlueprint already appends
	// validateExecTargets' dangling-target diagnostics.
	execSet, prog, partDiags := partitionBlueprint(bp, key)
	d.Items = append(d.Items, partDiags...)

	graphNodes, bpDefaults, validateDiags := validateBlueprint(bp, manifest, execSet)
	d.Items = append(d.Items, validateDiags...)
	if d.HasErrors() {
		return nil, &CompileError{Diagnostics: *d}
	}

	// No exec spine ⇒ no entrypoint to fire. Reject fail-loud (RC-7). A clean
	// partition with prog==nil here means the blueprint carried no exec node at
	// all (any partition error would have surfaced in d above).
	if prog == nil {
		d.AddError(ErrNoExecProgram,
			"blueprint produced no executable program — a stream rule needs an "+
				"entrypoint (on-start/on-tick/on-event) wired into an exec spine")
		return nil, &CompileError{Diagnostics: *d}
	}

	// Fold THIS blueprint's declared `variables[].value` constants ONCE (Orion
	// #192 / ADR 016 RC-6) — the same helper the scene loop calls. No reference
	// expander runs on this path, so there is no expander-harvested
	// bp.Defaults to merge and thus no double-fold: the declared variables are
	// seeded exactly here and nowhere else (ADR 017 §5 double-fold risk).
	foldDeclaredVariables(bpDefaults, bp.Variables)

	sorted, topoErr := topologicalSort(graphNodes, bp.Edges)
	if topoErr != nil {
		d.AddError(ErrTopologySort, "blueprint sort: %v", topoErr)
		return nil, &CompileError{Diagnostics: *d}
	}

	prefixGraphNodes(sorted, key)
	defaults := map[string]json.RawMessage{}
	for path, v := range bpDefaults {
		defaults[prefixDefaultLeaf(key, path)] = v
	}

	// Collect on-event topics + on-platform-event entry leaves from the exec
	// entrypoints — identical to the scene loop — then marshal the program.
	eventTopics := map[string]struct{}{}
	platformEntryLeaves := map[string]struct{}{}
	for _, e := range prog.Entrypoints {
		if e.Kind == "on-event" && e.Event != "" {
			eventTopics[e.Event] = struct{}{}
		}
		if e.Kind == "on-platform-event" && e.Event != "" {
			platformEntryLeaves[e.Event] = struct{}{}
		}
	}
	raw, mErr := marshalExecProgram(prog)
	if mErr != nil {
		d.AddError(ErrTopologySort, "exec program marshal: %v", mErr)
		return nil, &CompileError{Diagnostics: *d}
	}

	// Synthesize the platform-stream + event-topic acceptance bindings so a
	// blueprint-direct rule consumes data-plane event inputs exactly like a
	// scene (ADR 017 §3.1 parity). Layout adapters (extractAdapters) do not
	// exist here — there is no layout, the only legitimate divergence from the
	// scene path.
	var bindings []ExternalAdapter
	bindings = append(bindings, platformStreamBindings(sorted, platformEntryLeaves)...)
	bindings = append(bindings, eventTopicBindings(eventTopics, eventInputLeaves(sorted))...)

	graph := &Graph{
		SceneID:      bp.ID,
		Nodes:        sorted,
		Bindings:     bindings,
		Defaults:     defaults,
		ExecPrograms: []json.RawMessage{raw},
	}
	return graph, nil
}
