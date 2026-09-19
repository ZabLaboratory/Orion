package compiler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Compile turns a push envelope into a graph + bundle pair plus a
// content hash. On any error-severity diagnostic it returns
// (nil, nil, "", &CompileError{Diagnostics}); callers route on
// errors.As / CompileError.HasCode.
//
// The compiler is intentionally single-pass — no AST mutation post-
// validation, no second walk. Each step appends to diagnostics and
// returns early on hard errors that would make subsequent steps
// produce noise (e.g., a fetch failure means the rest of compile is
// skipped).
func Compile(
	ctx context.Context,
	sceneID string,
	envelope PushEnvelope,
	fetcher Fetcher,
) (*Graph, *RenderBundle, string, error) {

	if envelope.IsRollback() {
		// Rollback never recompiles — caller routes by IsRollback.
		// Returning here defends against accidental misuse.
		return nil, nil, "", fmt.Errorf("compiler: rollback path must not call Compile")
	}

	d := &Diagnostics{}

	// 1) Fetch layout, blueprint, components, compute manifest in
	//    parallel-friendly serial calls (HTTP keepalive amortises).
	layout, err := fetcher.FetchCanvasLayout(ctx, envelope.CanvasVersion)
	if err != nil {
		d.AddError(ErrFetchUpstream, "fetch canvas %s: %v", envelope.CanvasVersion, err)
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}
	// Normalise the dual-shape blueprint envelope into the single keyed
	// list the compiler walks (ADR 001 §3.1). The legacy singular
	// BlueBlueprintID folds to a length-1 list keyed "" (empty prefix →
	// byte-identical leaf paths to pre-001); "" / "none" / absent fold to
	// an empty list (blueprint-free scene, issue #28 robustness preserved —
	// the per-blueprint loop below runs zero times → zero-value graph).
	// A conflict (both singular AND plural set) is rejected at the API
	// layer (400 ENVELOPE_BLUEPRINT_CONFLICT) before Compile; re-deriving
	// here keeps Compile self-consistent and fails closed if ever misused.
	blueprintRefs, normErr := NormalizeBlueprints(envelope)
	if normErr != nil {
		d.AddError(ErrInvalidBinding, "blueprint envelope: %v", normErr)
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}
	// Keys must be unique within the envelope (§3.1/§5 R1) or the leaf-path
	// namespacing collides.
	if keyDiags := validateBlueprintKeys(blueprintRefs); len(keyDiags) > 0 {
		d.Items = append(d.Items, keyDiags...)
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}
	// Fetch each blueprint, keyed by its scene-local key. The fetcher
	// interface is unchanged (one id → one graph, §3.2); the loop is the
	// caller's. v1 keeps fetches serial (HTTP keepalive amortises, §5 R3).
	keyedBlueprints := make([]keyedBlueprint, 0, len(blueprintRefs))
	for _, ref := range blueprintRefs {
		bp, ferr := fetcher.FetchBlueprint(ctx, ref.ID)
		if ferr != nil {
			d.AddError(ErrFetchUpstream, "fetch blueprint %s (key %q): %v", ref.ID, ref.Key, ferr)
			return nil, nil, "", &CompileError{Diagnostics: *d}
		}
		// Expand blueprint-reference nodes (ADR 014) BEFORE manifest
		// validation / topo-sort / conformance: each `reference:
		// {blueprint_id, version}` node is replaced in-line, recursively, by
		// its pinned published sub-graph until the blueprint is flat core.*.
		// The per-blueprint validation loop below then sees only core.* —
		// byte-identical in nature to a scene authored without references, so
		// it (and the runtime) need no change. A reference-free blueprint is
		// returned unchanged (no reference node to expand → same graph).
		flat, expandDiags := expandReferences(ctx, bp, fetcher)
		if len(expandDiags) > 0 {
			d.Items = append(d.Items, expandDiags...)
			return nil, nil, "", &CompileError{Diagnostics: *d}
		}
		keyedBlueprints = append(keyedBlueprints, keyedBlueprint{key: ref.Key, graph: flat})
	}
	manifest, err := fetcher.FetchComputeManifest(ctx)
	if err != nil {
		d.AddError(ErrFetchUpstream, "fetch compute manifest: %v", err)
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}

	// Curated service-egress registry (ADR Blue 002 §3.2). Read off the
	// SAME manifest handshake via the optional FetchEgressRoutes method;
	// a fetcher that doesn't implement it leaves egress nil — fail-closed,
	// so any `core.service.call@1` node rejects EGRESS_ROUTE_NOT_DECLARED.
	var egress EgressRegistry
	if erf, ok := fetcher.(egressRouteFetcher); ok {
		egress, err = erf.FetchEgressRoutes(ctx)
		if err != nil {
			d.AddError(ErrFetchUpstream, "fetch egress routes: %v", err)
			return nil, nil, "", &CompileError{Diagnostics: *d}
		}
	}

	components := make(map[string]*UserComponent, len(envelope.Components))
	for _, ref := range envelope.Components {
		uc, err := fetcher.FetchComponent(ctx, ref)
		if err != nil {
			d.AddError(ErrFetchUpstream, "fetch component %s@%s: %v", ref.ID, ref.Version, err)
			continue
		}
		components[ref.ID] = uc
	}
	if d.HasErrors() {
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}

	// 2) Cycle detection over the component-uses-component graph
	//    (criterion 17). Layout root is allowed to use any component;
	//    components themselves form a closed graph among the supplied set.
	if cyc := detectComponentCycles(components); len(cyc) > 0 {
		d.AddError(ErrCyclicComponent, "component cycle: %s", strings.Join(cyc, " → "))
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}

	// 3) Inline user components + hoist operator_inputs. The bundle's
	//    root is the layout with every component reference replaced by
	//    its body (parameters substituted, instance-paths prefixed).
	expanded, hoistedInputs, hoistErrs := expandLayout(layout.Root, components, "")
	d.Items = append(d.Items, hoistErrs...)
	if d.HasErrors() {
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}

	// Top-level operator_inputs (declared on the layout itself) come first.
	allInputs := make([]OperatorInput, 0, len(layout.Inputs)+len(hoistedInputs))
	allInputs = append(allInputs, layout.Inputs...)
	allInputs = append(allInputs, hoistedInputs...)
	if dups := duplicateInputPaths(allInputs); len(dups) > 0 {
		d.AddError(ErrInvalidOperatorInput, "duplicate operator_input paths: %s", strings.Join(dups, ", "))
	}

	// 4+5) Validate purity (criterion 18) and topo-sort PER BLUEPRINT, then
	//    concatenate in stable key order (ADR 001 §3.2). Each blueprint's
	//    leaf paths, node ids and Defaults keys are prefixed by "<key>."
	//    (§3.3) so two blueprints declaring the same leaf name do not collide
	//    in one state namespace. The legacy key "" yields an empty prefix →
	//    paths/ids/defaults are byte-identical to the single-blueprint world
	//    (R4 non-regression). Edges are intra-blueprint (v1 forbids
	//    cross-blueprint edges, §3.4), so each graph topo-sorts independently.
	var sorted []GraphNode
	defaults := map[string]json.RawMessage{}
	// execPrograms collects one compiled ExecProgram per exec-bearing
	// blueprint (ADR 006 §3.1). keyedBlueprints is already in stable key
	// order (ADR 001 §3.2), so appending here yields the deterministic
	// blueprint-key order the artefact requires (criterion #2). A
	// blueprint with no exec node contributes nothing — a pure-dataflow
	// scene leaves execPrograms empty → graph.ExecPrograms stays nil
	// (omitempty) → byte-identical artefact to pre-lift.
	var execPrograms []json.RawMessage
	// eventTopics accumulates every distinct on-event topic across all
	// exec-bearing blueprints (issue #148, ADR 008 §3.3). Each becomes a
	// synthesized `event-topic` acceptance binding below — the mirror of
	// platformStreamBindings — so sceneAcceptsPath routes an operator /
	// service write to `__events.<topic>` to the scene. Topics are a flat
	// global namespace (the runtime indexes execOnEvent by the raw event
	// name, scene.go:657), so they are NOT key-prefixed.
	eventTopics := map[string]struct{}{}
	// platformEntryLeaves accumulates the `__inputs.platform.*` leaves
	// carried by `on-platform-event` ExecEntries (ADR 013 §3.6). They join
	// the platform leaves expanded by quasar.* dataflow nodes in the
	// platformStreamBindings acceptance set — ADDITIVE: a scene may have an
	// entry on a leaf no quasar.* node references, and it must still be
	// accepted (the entry IS what arms the spine on the Quasar write).
	platformEntryLeaves := map[string]struct{}{}
	for _, kb := range keyedBlueprints {
		// Partition first: the exec node set drives both the data-node
		// exclusion (validateBlueprint) and the ExecProgram emission.
		execSet, prog, partDiags := partitionBlueprint(kb.graph, kb.key)
		d.Items = append(d.Items, partDiags...)

		graphNodes, bpDefaults, validateDiags := validateBlueprint(kb.graph, manifest, execSet)
		d.Items = append(d.Items, validateDiags...)

		// Resolve every `core.service.call@1` exec node against the curated
		// egress registry (ADR 002 §3.2): an undeclared (service, route_id)
		// is a structural compile reject (EGRESS_ROUTE_NOT_DECLARED), a
		// declared one bakes its method/path_template/token_paths into the
		// node Config so the runtime builds the path + scopes the token from
		// CURATED data, never from the authored graph (closes §3.6.A).
		d.Items = append(d.Items, resolveEgressRoutes(prog, egress)...)
		if d.HasErrors() {
			return nil, nil, "", &CompileError{Diagnostics: *d}
		}

		// Fold the `__vars..` constant seeds the reference expander harvested
		// from inlined functions' `variables[].value` (Orion #192) into this
		// blueprint's defaults. They are in the empty-key `__vars..<var>` form
		// (varsLeaf), so prefixDefaultLeaf below substitutes the blueprint key
		// INSIDE the prefix — byte-identical to the leaf prefixGraphNodes wrote
		// on the reading `core.variable.get@1`. A blueprint with no such seed
		// leaves bpDefaults untouched (kb.graph.Defaults nil).
		for path, v := range kb.graph.Defaults {
			bpDefaults[path] = v
		}

		// Fold THIS top-level blueprint's own declared `variables[].value`
		// constants (Orion #192 / ADR 016 RC-6). The expander above only
		// harvested INLINED references' variables into kb.graph.Defaults; a
		// reference-free top-level blueprint (e.g. one declaring `palette`)
		// never ran the expander, so without this its `palette` leaf is never
		// seeded → score-to-color resolves null (e2e #152). Shared with the
		// in-body simulate path so the two can never drift. Value-less (pure
		// mutable shared-state) variables are skipped — they reseed from
		// declared defaults on activation (invariant ADR 003/006), they are not
		// compile-time constants. bpDefaults is key-namespaced below by
		// prefixDefaultLeaf, so the empty-key `__vars..<name>` form is correct.
		foldDeclaredVariables(bpDefaults, kb.graph.Variables)

		bpSorted, topoErr := topologicalSort(graphNodes, kb.graph.Edges)
		if topoErr != nil {
			d.AddError(ErrTopologySort, "blueprint %q sort: %v", kb.key, topoErr)
			return nil, nil, "", &CompileError{Diagnostics: *d}
		}

		// Prefix this blueprint's contributions by "<key>." (empty for legacy).
		prefixGraphNodes(bpSorted, kb.key)
		sorted = append(sorted, bpSorted...)
		for path, v := range bpDefaults {
			defaults[prefixDefaultLeaf(kb.key, path)] = v
		}

		if prog != nil {
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
				d.AddError(ErrTopologySort, "blueprint %q exec program marshal: %v", kb.key, mErr)
				return nil, nil, "", &CompileError{Diagnostics: *d}
			}
			execPrograms = append(execPrograms, raw)
		}
	}

	// Seed graph.Defaults from operator_input declarations (M9). Until now
	// Defaults was fed ONLY by blueprint constant sources / unwired ports —
	// an operator_input's leaf was declared (OperatorInputs) but never given
	// its boot value, so criterion 11 (restart reseeds from declared
	// defaults) did not hold for operator inputs and the first capture saw
	// the leaf absent. Each input carrying a `default` seeds its own leaf.
	// allInputs paths are already instance-prefixed by expandLayout (hoisted)
	// / authored verbatim (top-level), so the leaf address matches what the
	// runtime writes and what sceneAcceptsPath registers. An input without a
	// default leaves the leaf unseeded (no value at cold start, unchanged).
	for _, in := range allInputs {
		if len(in.Default) == 0 {
			continue
		}
		defaults[in.Path] = in.Default
	}

	// Seed graph.Defaults from the LAYOUT's literal map. Static text/image/media
	// authored in Canvas bind to `__lit.<kind>.<id>` leaves whose constant VALUE
	// lives in layout.Defaults (see CanvasLayout.Defaults). These are the only
	// source for a transcribed scene's labels/photos; without them the bound
	// components paint empty (the gray-canvas symptom). Layout-global, seeded
	// verbatim. A blueprint/operator default already set above wins (data
	// overrides a static literal), so only absent keys are filled.
	for path, v := range layout.Defaults {
		if _, exists := defaults[path]; !exists {
			defaults[path] = v
		}
	}

	// Validate that every component binding addressing the blueprint-key
	// namespace names a DECLARED key (ADR 001 §3.3.4): in a multi-blueprint
	// scene (≥1 non-empty key) a dotted binding's leading segment must be a
	// declared key, else UNKNOWN_BLUEPRINT_KEY. Legacy/blueprint-free scenes
	// (only the "" key, or none) skip this — their bindings are keyless and
	// must stay byte-identical to today.
	if bindDiags := validateBindingKeys(layout, components, declaredKeys(blueprintRefs)); len(bindDiags) > 0 {
		d.Items = append(d.Items, bindDiags...)
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}

	// 6) Extract external adapters declared in bindings (HTTP poll,
	//    pg-listen, platform-stream, tick) — done by walking nodes
	//    whose compute is one of the adapter kinds. v1 keeps this
	//    rule simple: adapter declarations live in the layout's
	//    `external_adapters` block (Canvas-side authored), the
	//    compiler echoes them through after validation.
	adapters := extractAdapters(layout, &allInputs, d)

	// 6b) Synthesize one `platform-stream` binding per distinct platform
	//     leaf the blueprints expanded (ADR 003 §3.3.3, issue #84) so the
	//     inbox's sceneAcceptsPath accepts Quasar's writes. Acceptance
	//     declaration only — no goroutine is ever spawned for this Kind.
	adapters = append(adapters, platformStreamBindings(sorted, platformEntryLeaves)...)

	// 6c) Synthesize one `event-topic` binding per distinct on-event topic
	//     (issue #148, ADR 008 §3.3) — the exact mirror of (6b): pure
	//     acceptance so sceneAcceptsPath routes a write to `__events.<topic>`
	//     to the scene. No goroutine is ever spawned for this Kind. Topics
	//     sorted for scene_version hash determinism (criterion #6).
	//
	//     ADDITIVE (ADR 013, the quasar-finale 5th link): a PURE-DATAFLOW
	//     scene reads an `__events.*` topic through a `core.input@1` leaf node
	//     (the M1 shape, no on-event exec entry) — its leaf address rides on a
	//     data GraphNode.Path, NOT on an ExecEntry, so the eventTopics scan
	//     above (fed only by on-event entrypoints) misses it and NO acceptance
	//     binding is synthesized → sceneAcceptsPath rejects the wire/service
	//     write → the leaf is never written → the dataflow cone never wakes.
	//     This is the exact mirror of (6b)'s entryLeaves merge for platform
	//     leaves: scan the compiled data nodes for `__events.*` paths and union
	//     them into the topic set so the input-driven case gets the same
	//     acceptance binding. A scene with BOTH an on-event entry and a
	//     dataflow input on the same topic dedups to one binding.
	adapters = append(adapters, eventTopicBindings(eventTopics, eventInputLeaves(sorted))...)

	// 6d) Resolve every `core.source.read@1` node's `source_id` against the
	//     assembled adapter set and fold the introspection descriptor into
	//     the node config under `__resolved_source` (ADR 012 §1.2, Option B).
	//     An undeclared source is a STRUCTURAL push-time reject
	//     (SOURCE_NOT_DECLARED, §1.4) — source.read is a pure compute with no
	//     error port, so resolution happens here, never on air. Run after all
	//     adapters (incl. synthesized) exist and before the error gate below.
	resolveSourceReads(sorted, adapters, d)

	if d.HasErrors() {
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}

	// 7) Assemble artefacts. SceneVersion comes from a hash over the
	//    canonical bytes of both — see scene_version.go.
	graph := &Graph{
		SceneID:        sceneID,
		Nodes:          sorted,
		Bindings:       adapters,
		Defaults:       defaults,
		OperatorInputs: allInputs,
		// ExecPrograms is nil for a pure-dataflow scene (omitempty → the
		// artefact and its scene_version hash are byte-identical to
		// pre-lift, ADR 006 §3.1 / criterion #2). Emit-but-never-install:
		// the runtime install path is untouched (ADR 006 §3.7).
		ExecPrograms: execPrograms,
	}
	// Lower the authoring-vocab tree (`style.*`, `size.{w,h}`, `geometry`,
	// `cornerRadius`, nested `stroke`) into the FLAT render vocab the
	// Lumencast runtime reads (`size`/`weight`/`colour`, `width`/`height`,
	// `kind`/`radius`, `stroke`+`stroke_width`). The lowering is the
	// missing LSML→RenderBundle step (ADR 007 §9); without it Solar paints
	// at default font/size/dims. It operates on a fresh tree so `expanded`
	// stays in authoring vocab for EmitLSML (the LSML bundle keeps the
	// authoring keys — no double-lowering, ADR 007 §9.6). Ref #41.
	// Parse the inlined Animation Asset catalogue (ADR 011 §3.1 / I1) so the
	// lowering can resolve `animation.play.animation_id` → asset → keyframe
	// node at compile time (§3.3/§3.4). A nil/malformed catalogue yields an
	// empty map → every `animation` element falls through inert. Asset
	// resolution is purely local to the served layout (no cross-service
	// fetch, ADR 011 §5 R1 mitigation / R4: no new Bastion surface).
	animations := parseAnimationCatalogue(layout.Animations)
	loweredRoot := lowerRenderTree(expanded, animations)
	bundle := &RenderBundle{
		Root:             loweredRoot,
		OperatorInputs:   allInputs,
		ExternalAdapters: adapters,
		// Carry the pre-lowering authoring tree so EmitLSML (the C4 path)
		// reads the authoring vocab, not the lowered render vocab. It is
		// `json:"-"` so it never reaches the wire/persisted bundle/hash —
		// `Root` served to Solar stays lowered (fidelity #41 intact).
		// `expanded` is a distinct object from `loweredRoot` (lowering
		// returned a fresh tree), so they never alias. Fixes Vigil's
		// finding on PR #42 (EmitLSML was fed bundle.Root lowered).
		AuthoringRoot: expanded,
		// Carry the bundle-level asset block (allowedHosts/fonts/preload)
		// verbatim from the authoring layout to EmitLSML, which preserves it
		// on the LSML bundle (ADR 002 §3.4 T6). Opaque passthrough — Orion
		// neither fabricates a host nor strips the block; nil when unauthored.
		LSMLAssets: layout.Assets,
	}

	version, err := computeSceneVersion(graph, bundle)
	if err != nil {
		d.AddError(ErrTopologySort, "scene version hash: %v", err)
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}
	graph.SceneVersion = version
	bundle.SceneVersion = version

	return graph, bundle, version, nil
}

// keyedBlueprint pairs a fetched blueprint graph with the scene-local key
// it was fetched under (ADR 001 §3.2). The key drives leaf-path prefixing.
type keyedBlueprint struct {
	key   string
	graph *BlueprintGraph
}

// prefixLeaf prefixes a leaf path with "<key>." (ADR 001 §3.3). The legacy
// key "" yields the path unchanged → byte-identical leaf addresses to the
// pre-001 single-blueprint world (R4 non-regression). An empty path (an
// intermediate compute with no public leaf) stays empty.
func prefixLeaf(key, path string) string {
	if key == "" || path == "" {
		return path
	}
	return key + "." + path
}

// prefixDefaultLeaf key-namespaces a DEFAULT leaf address the same three ways
// prefixGraphNodes namespaces a node's runtime leaf — so a default seed always
// lands on the byte-identical address its reading node binds to:
//
//   - `__inputs.platform.*` : exempt (global Quasar address, never prefixed).
//   - `__vars.*`            : the blueprint key goes INSIDE the prefix
//     (`__vars..<v>` → `__vars.<key>.<v>`), matching execVariableSet's write
//     and the `core.variable.get@1` leaf prefixGraphNodes rewrites — NOT the
//     front-prefixed `<key>.__vars..<v>` plain prefixLeaf would produce.
//   - everything else        : plain `<key>.` front-prefix (prefixLeaf).
//
// The legacy key "" is a no-op for all three (prefixLeaf returns the path; the
// `__vars` branch reduces to inserting "" → the unchanged `__vars..<v>` form).
// Pre-#192 defaults (literal leaves, unwired-port fallbacks) are node-id-based
// and never carry the `__vars.`/`__inputs.platform.` prefixes, so they take the
// default branch — byte-identical to the prior `prefixLeaf(key, path)`.
func prefixDefaultLeaf(key, path string) string {
	switch {
	case key == "" || path == "":
		return path
	case strings.HasPrefix(path, platformLeafPrefix):
		return path // global Quasar address — exempt (issue #84)
	case strings.HasPrefix(path, varsLeafPrefix):
		rest := path[len(varsLeafPrefix):] // ".<name>"
		return varsLeafPrefix + key + rest
	default:
		return prefixLeaf(key, path)
	}
}

// prefixGraphNodes namespaces a blueprint's runtime nodes by its key in place
// (ADR 001 §3.3): both the node id (so two blueprints' node ids never collide
// in the merged graph) and the public leaf Path, plus the Upstream id
// references (which point at sibling node ids within the same blueprint, so
// they take the same prefix). The legacy key "" is a no-op.
func prefixGraphNodes(nodes []GraphNode, key string) {
	if key == "" {
		return
	}
	for i := range nodes {
		nodes[i].ID = key + "." + nodes[i].ID
		// Platform leaves are exempt from key-prefixing (issue #84):
		// `__inputs.platform.*` is the GLOBAL address Quasar writes to —
		// a cross-repo byte-contract with Blue/Quasar that a scene-local
		// blueprint key must not rewrite. The node id above still takes
		// the prefix (ids are scene-internal).
		switch {
		case strings.HasPrefix(nodes[i].Path, platformLeafPrefix):
			// Platform leaves are exempt (see above) — global Quasar address.
		case strings.HasPrefix(nodes[i].Path, varsLeafPrefix):
			// `__vars` leaves (variable.get) namespace the blueprint key
			// INSIDE the prefix so the read matches execVariableSet's write
			// `__vars.<key>.<name>`, not the front-prefixed
			// `<key>.__vars.<name>` that prefixLeaf would produce. nodeLeafPath
			// emitted the empty-key form `__vars..<name>` (leading "." after
			// the prefix); we substitute the real key into that empty segment.
			rest := nodes[i].Path[len(varsLeafPrefix):] // ".<name>"
			nodes[i].Path = varsLeafPrefix + key + rest
		default:
			nodes[i].Path = prefixLeaf(key, nodes[i].Path)
		}
		for j := range nodes[i].Upstream {
			nodes[i].Upstream[j] = key + "." + nodes[i].Upstream[j]
		}
		// Named inputs reference the same sibling node ids (issue #79) —
		// they take the same prefix; port names are never prefixed (they
		// address the node's own port set, not the state namespace).
		for j := range nodes[i].Inputs {
			nodes[i].Inputs[j].From = key + "." + nodes[i].Inputs[j].From
		}
	}
}

// validateBindingKeys enforces the component↔blueprint binding rule
// (ADR 001 §3.3.4). It only engages when the scene declares at least one
// NON-EMPTY blueprint key (a genuine multi-blueprint scene); legacy and
// blueprint-free scenes (only the "" key, or none) are exempt so their
// keyless bindings stay byte-identical to today.
//
// When engaged: every dotted binding value on the layout (and on every
// fetched component body) whose leading segment is non-empty must name a
// DECLARED blueprint key — otherwise it is a typo'd / dangling reference and
// the push fails closed with UNKNOWN_BLUEPRINT_KEY. A single-segment binding
// (no dot) is a keyless non-blueprint binding and is left alone.
func validateBindingKeys(layout *CanvasLayout, components map[string]*UserComponent, keys map[string]struct{}) []Diagnostic {
	hasNonEmptyKey := false
	for k := range keys {
		if k != "" {
			hasNonEmptyKey = true
			break
		}
	}
	if !hasNonEmptyKey {
		return nil
	}

	var diags []Diagnostic
	seen := make(map[string]struct{})
	check := func(n LayoutNode) {
		for _, v := range n.Bindings {
			lead, _, dotted := strings.Cut(v, ".")
			if !dotted || lead == "" {
				continue // keyless non-blueprint binding
			}
			if _, ok := keys[lead]; ok {
				continue // resolves to a declared blueprint key
			}
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
			diags = append(diags, Diagnostic{
				Code:     ErrUnknownBlueprintKey,
				Severity: "error",
				Message:  fmt.Sprintf("binding %q references undeclared blueprint key %q", v, lead),
				Path:     v,
			})
		}
	}
	if layout != nil {
		walkLayout(layout.Root, check)
	}
	for _, uc := range components {
		if uc != nil {
			walkLayout(uc.Body, check)
		}
	}
	return diags
}

// detectComponentCycles walks the component-uses-component graph. A
// node X uses Y when X's body's tree includes a LayoutNode whose
// kind == Y's id. Returns the offending cycle as a list of ids; nil
// if no cycle. Algorithm: DFS with grey/black colouring.
func detectComponentCycles(components map[string]*UserComponent) []string {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := make(map[string]int, len(components))
	for id := range components {
		colour[id] = white
	}

	var (
		stack []string
		out   []string
	)
	var dfs func(id string) bool
	dfs = func(id string) bool {
		c, exists := components[id]
		if !exists {
			return false
		}
		if colour[id] == grey {
			// found a back-edge — cycle.
			start := -1
			for i, s := range stack {
				if s == id {
					start = i
					break
				}
			}
			if start >= 0 {
				out = append(out, stack[start:]...)
				out = append(out, id)
			}
			return true
		}
		if colour[id] == black {
			return false
		}
		colour[id] = grey
		stack = append(stack, id)
		var hit bool
		walkLayout(c.Body, func(n LayoutNode) {
			if hit {
				return
			}
			if _, used := components[n.Kind]; used && n.Kind != id {
				if dfs(n.Kind) {
					hit = true
				}
			}
		})
		stack = stack[:len(stack)-1]
		colour[id] = black
		return hit
	}

	for id := range components {
		if colour[id] == white {
			if dfs(id) {
				return out
			}
		}
	}
	return nil
}

// expandLayout walks the layout tree, inlining every user-component
// reference at its position. The instance path is the dotted address
// of the node from root (e.g., `root.children.0.children.2.team-row-0`)
// — operator_inputs declared on the component get re-pathed under
// this prefix so multiple instances don't collide.
func expandLayout(node LayoutNode, components map[string]*UserComponent, instancePath string) (LayoutNode, []OperatorInput, []Diagnostic) {
	var diags []Diagnostic

	// If this node *is* a component reference, replace it.
	if uc, isComp := components[node.Kind]; isComp {
		// Substitute parameters into a copy of the body.
		inlined := substituteComponentArgs(uc.Body, uc.Parameters, node.ComponentArgs)
		// Recurse into the inlined body in case it itself uses
		// other components.
		inlined, hoisted, childDiags := expandLayout(inlined, components, joinPath(instancePath, node.ID, node.Kind))
		diags = append(diags, childDiags...)

		// Hoist the component's own operator_inputs prefixed with
		// the instance path so two `team-row` widgets don't share
		// `path` collisions.
		for _, in := range uc.Inputs {
			cp := in
			cp.Path = joinPath(instancePath, node.ID, in.Path)
			hoisted = append(hoisted, cp)
		}
		return inlined, hoisted, diags
	}

	// Plain primitive (or unknown kind that isn't a component).
	out := node
	out.Children = nil
	var hoisted []OperatorInput
	for _, child := range node.Children {
		expanded, h, d := expandLayout(child, components, joinPath(instancePath, node.ID, ""))
		out.Children = append(out.Children, expanded)
		hoisted = append(hoisted, h...)
		diags = append(diags, d...)
	}
	return out, hoisted, diags
}

// substituteComponentArgs walks body and replaces any binding pointing
// to a parameter name with the supplied argument expression.
func substituteComponentArgs(body LayoutNode, params []ComponentParam, args map[string]json.RawMessage) LayoutNode {
	paramSet := make(map[string]json.RawMessage, len(params))
	for _, p := range params {
		if p.Default != nil {
			paramSet[p.Name] = p.Default
		}
	}
	for k, v := range args {
		paramSet[k] = v
	}
	var walk func(n LayoutNode) LayoutNode
	walk = func(n LayoutNode) LayoutNode {
		out := n
		// For each binding whose value matches a parameter name,
		// rewrite to the parameter's bound *path* (string args
		// representing a state path). Non-string args become props.
		if len(n.Bindings) > 0 {
			out.Bindings = make(map[string]string, len(n.Bindings))
			for k, v := range n.Bindings {
				if raw, ok := paramSet[v]; ok {
					var s string
					if err := json.Unmarshal(raw, &s); err == nil {
						out.Bindings[k] = s
						continue
					}
				}
				out.Bindings[k] = v
			}
		}
		if len(n.Props) > 0 {
			out.Props = make(map[string]json.RawMessage, len(n.Props))
			for k, v := range n.Props {
				out.Props[k] = v
			}
		}
		if len(n.Children) > 0 {
			out.Children = make([]LayoutNode, len(n.Children))
			for i := range n.Children {
				out.Children[i] = walk(n.Children[i])
			}
		}
		return out
	}
	return walk(body)
}
