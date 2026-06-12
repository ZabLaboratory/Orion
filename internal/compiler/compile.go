package compiler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
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
		keyedBlueprints = append(keyedBlueprints, keyedBlueprint{key: ref.Key, graph: bp})
	}
	manifest, err := fetcher.FetchComputeManifest(ctx)
	if err != nil {
		d.AddError(ErrFetchUpstream, "fetch compute manifest: %v", err)
		return nil, nil, "", &CompileError{Diagnostics: *d}
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
	for _, kb := range keyedBlueprints {
		// Partition first: the exec node set drives both the data-node
		// exclusion (validateBlueprint) and the ExecProgram emission.
		execSet, prog, partDiags := partitionBlueprint(kb.graph, kb.key)
		d.Items = append(d.Items, partDiags...)

		graphNodes, bpDefaults, validateDiags := validateBlueprint(kb.graph, manifest, execSet)
		d.Items = append(d.Items, validateDiags...)
		if d.HasErrors() {
			return nil, nil, "", &CompileError{Diagnostics: *d}
		}

		bpSorted, topoErr := topologicalSort(graphNodes, kb.graph.Edges)
		if topoErr != nil {
			d.AddError(ErrTopologySort, "blueprint %q sort: %v", kb.key, topoErr)
			return nil, nil, "", &CompileError{Diagnostics: *d}
		}

		// Prefix this blueprint's contributions by "<key>." (empty for legacy).
		prefixGraphNodes(bpSorted, kb.key)
		sorted = append(sorted, bpSorted...)
		for path, v := range bpDefaults {
			defaults[prefixLeaf(kb.key, path)] = v
		}

		if prog != nil {
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
	adapters = append(adapters, platformStreamBindings(sorted)...)

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
	loweredRoot := lowerRenderTree(expanded)
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
		if !strings.HasPrefix(nodes[i].Path, platformLeafPrefix) {
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

// validateBlueprint checks every node's compute is in Blue's manifest
// and builds the DATA-layer graph nodes plus a defaults map seeded from
// declared inputs. EXEC-layer nodes (those in execSet — any node with an
// exec pin, ADR 006 §3.1) are skipped: they are routed to the blueprint's
// ExecProgram by partitionBlueprint, never recomputed as data nodes
// (closing the silent-skip hole, ADR 006 §1). Purity is NO LONGER a
// rejection gate (ADR 006 §3.2): an impure data compute is served, not
// refused — capability is total, proof is the validation gate's job.
func validateBlueprint(b *BlueprintGraph, manifest ComputeManifest, execSet map[string]struct{}) ([]GraphNode, map[string]json.RawMessage, []Diagnostic) {
	var diags []Diagnostic
	defaults := map[string]json.RawMessage{}
	nodes := make([]GraphNode, 0, len(b.Nodes))

	// Upstream ids and named inputs are built off the SAME edge walk so
	// they stay zipped 1:1 (issue #79): Upstream[i] == Inputs[i].From for
	// every i. Inputs carries the edge's to_port verbatim — the runtime
	// delivers each upstream value under that declared name.
	upstreams := make(map[string][]string)
	inputs := make(map[string][]GraphInput)
	for _, e := range b.Edges {
		upstreams[e.ToNode] = append(upstreams[e.ToNode], e.FromNode)
		inputs[e.ToNode] = append(inputs[e.ToNode], GraphInput{From: e.FromNode, Port: e.ToPort})
	}

	for _, n := range b.Nodes {
		// (a) Exec-layer nodes (ADR 006 §3.1) are routed to the ExecProgram
		// by partitionBlueprint; they are NOT data nodes and must not
		// appear in the GraphNode list. Skipping them here (and their
		// edges fall away in topologicalSort, which drops edges to
		// dropped nodes) closes the silent-skip hole. This check runs
		// FIRST so exec nodes bypass the manifest gate.
		if _, isExec := execSet[n.ID]; isExec {
			continue
		}

		// ADR 007 §3.3: the former structural guard (DB_NODE_OUTSIDE_QUERY)
		// is retired. The six core.db.* clause atomics are ordinary pure
		// computes (KindCompute, compute_db.go) composable in the main
		// graph; they flow through the manifest lookup below like any other
		// data node.

		// (c) Manifest validation: the compute must be known to Blue.
		// Purity is NO LONGER a rejection gate (ADR 006 §3.2): an impure
		// data compute is served, not refused.
		entry, found := manifest[n.Compute]
		if !found {
			diags = append(diags, Diagnostic{
				Code:     ErrUnknownComputeNode,
				Severity: "error",
				Message:  fmt.Sprintf("blueprint node %s references unknown compute %q", n.ID, n.Compute),
				Path:     n.ID,
			})
			continue
		}

		// Derive kind + leaf path from the REAL node body (config /
		// inputs / outputs), per ADR 004 §7.2. The leaf-path rule is
		// isolated in nodeLeafPath — it is the single seam Atlas flagged
		// for the residual question (the exact config key Prism's
		// blueprint editor authors for an output name). If that ever
		// diverges from the stdlib signature, this one function changes,
		// not the struct shape.
		//
		// Platform-event nodes (`quasar.<platform>.<event>@N`, ADR 003
		// §3.3.3 / issue #84) take the dedicated expansion instead: their
		// leaf is the GLOBAL Quasar-written address derived from the node
		// name + authored config.channel, byte-identical to Blue's
		// declared `signature.platform.leaf_path` and to Quasar's
		// `leaf_path(event)`.
		var path string
		if platform, event, isPlatform := platformNodeRef(n.Compute); isPlatform {
			leaf, pd := platformLeafPath(n, platform, event)
			if pd != nil {
				diags = append(diags, *pd)
				continue
			}
			path = leaf
		} else {
			path = nodeLeafPath(n)
		}

		kind := "computed"
		switch {
		case n.Compute == coreOutput:
			// An explicit output sink — the leaf the runtime writes to.
			kind = "output"
		case len(upstreams[n.ID]) == 0:
			// A leaf with no upstream: an adapter/operator input
			// (core.input@1) or a constant source (core.literal@1).
			kind = "input"
		}

		// Seed graph.Defaults from constant sources and unwired ports.
		// core.literal@1's config.value is the constant; it seeds the
		// literal's own output leaf (replaces the old Args["default"]).
		if n.Compute == coreLiteral && path != "" {
			if v, ok := n.Config["value"]; ok {
				defaults[path] = v
			}
		}
		// Unwired input ports seed their declared fallback so a node
		// whose port has no inbound edge still has a value at cold start.
		for _, p := range n.Inputs {
			if p.Default == nil {
				continue
			}
			if _, wired := wiredPorts(n.ID, b.Edges)[p.Name]; wired {
				continue
			}
			defaults[n.ID+"."+p.Name] = p.Default
		}

		// Carry config to the runtime for COMPUTED nodes only (issue
		// #81): Blue's handlers receive (inputs, config) and the pure
		// data tranche needs it (get-field/set-field `path`, aggregate
		// `op`). input/output/literal configs are already lowered into
		// Path / Defaults above — carrying them again would only churn
		// the scene_version hash for nothing.
		var cfg map[string]json.RawMessage
		if kind == "computed" && len(n.Config) > 0 {
			cfg = n.Config
		}

		nodes = append(nodes, GraphNode{
			ID:        n.ID,
			Kind:      kind,
			Path:      path,
			Compute:   n.Compute,
			Upstream:  upstreams[n.ID],
			Inputs:    inputs[n.ID],
			IsPure:    entry.IsPure,
			IsBounded: entry.IsBounded,
			Config:    cfg,
		})
	}
	return nodes, defaults, diags
}

// Stdlib node references whose body carries a state-leaf-bearing config
// (ADR 004 §7.2, source: Blue/src/blue/services/stdlib_seeder.py).
const (
	coreOutput  = "core.output@1"  // config.name → the leaf the runtime writes
	coreInput   = "core.input@1"   // config.name → the interface input name
	coreLiteral = "core.literal@1" // config.value → seeds graph.Defaults
)

// nodeLeafPath returns the state leaf a blueprint node's result is
// written to, or "" for an intermediate compute whose outputs only feed
// downstream nodes via edges (core.math.*, core.compare.*, …).
//
// This is the leaf-path rule that replaces the phantom OutputAt
// (ADR 004 §7.2). It is deliberately the ONLY place the wire body is
// translated into a leaf address, so the residual question — the exact
// config key Prism's blueprint editor writes for an output's name — has
// a single seam to adjust if a real Prism-authored blueprint ever
// diverges from the stdlib signature.contract.
//
// Sink nodes (core.output@1 / core.input@1) name their leaf in
// config.name. A literal (core.literal@1) has no config.name; its output
// leaf is the node's own id (matching scene.go's upstreamPath fallback,
// which addresses an unnamed upstream node by its id). All other nodes
// return "" — their outputs are consumed off edges, never as leaves.
func nodeLeafPath(n BlueprintNode) string {
	switch n.Compute {
	case coreOutput, coreInput:
		if raw, ok := n.Config["name"]; ok {
			var name string
			if err := json.Unmarshal(raw, &name); err == nil && name != "" {
				return name
			}
		}
		return ""
	case coreLiteral:
		// A literal seeds Defaults at its own output leaf; the runtime
		// addresses an unnamed upstream by node id (scene.go:362-371).
		return n.ID
	default:
		return ""
	}
}

// Platform-event leaf binding (ADR 003 §3.3.3, issue #84).
//
// platformLeafPrefix is the global namespace Quasar writes into. Leaves
// under it are NEVER blueprint-key-prefixed (prefixGraphNodes skips
// them): the address is a cross-repo contract — Blue declares it,
// Quasar computes it, Orion must expand to the byte-identical string —
// so it cannot vary with a scene-local blueprint key.
const platformLeafPrefix = "__inputs.platform."

// platformChannelRE is the canonical channel charset (ADR 005 §9 /
// ADR 003 §3.3). Validation runs AFTER casefolding: pure-case variants
// of a valid handle are folded, anything else is rejected — never
// rewritten. The regex guarantees the folded channel is ASCII, on which
// Go's strings.ToLower and Python's str.lower agree byte-for-byte, so
// Orion's expansion matches Quasar's.
var platformChannelRE = regexp.MustCompile(`^[a-z0-9_]+$`)

// platformNodeRef reports whether compute names a quasar platform-event
// node (`quasar.<platform>.<event>@<version>`) and, if so, returns its
// platform and event-type segments. The event list itself is owned by
// Blue's manifest (CANONICAL_EVENT_TYPES → the 14 `quasar.twitch.*@1`
// entries today); validateBlueprint's manifest gate has already
// rejected unknown computes before this runs, so no Orion-side
// allowlist is duplicated here.
func platformNodeRef(compute string) (platform, event string, ok bool) {
	rest, found := strings.CutPrefix(compute, "quasar.")
	if !found {
		return "", "", false
	}
	rest, _, _ = strings.Cut(rest, "@")
	platform, event, found = strings.Cut(rest, ".")
	if !found || platform == "" || event == "" {
		return "", "", false
	}
	return platform, event, true
}

// platformLeafPath expands one platform node's authored config.channel
// into the canonical leaf `__inputs.platform.<platform>.<channel>.last_<event>`
// (ADR 003 §3.3.2). Channel handling is casefold-THEN-validate: a
// `ZabChannel` folds to `zabchannel`; a `Zab-Channel` is rejected
// (PLATFORM_CHANNEL_INVALID), not rewritten. Both diagnostics are
// structural authoring errors on the node's config — not capability
// rejections of the node type (§3.3.3).
func platformLeafPath(n BlueprintNode, platform, event string) (string, *Diagnostic) {
	raw, ok := n.Config["channel"]
	if !ok {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelMissing,
			Severity: "error",
			Message:  fmt.Sprintf("platform node %s (%s) declares no config.channel — cannot expand its %s<%s>.last_%s leaf", n.ID, n.Compute, platformLeafPrefix, platform, event),
			Path:     n.ID,
		}
	}
	var channel string
	if err := json.Unmarshal(raw, &channel); err != nil {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelInvalid,
			Severity: "error",
			Message:  fmt.Sprintf("platform node %s (%s): config.channel must be a JSON string", n.ID, n.Compute),
			Path:     n.ID,
		}
	}
	if channel == "" {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelMissing,
			Severity: "error",
			Message:  fmt.Sprintf("platform node %s (%s): config.channel is empty", n.ID, n.Compute),
			Path:     n.ID,
		}
	}
	folded := strings.ToLower(channel)
	if !platformChannelRE.MatchString(folded) {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelInvalid,
			Severity: "error",
			Message:  fmt.Sprintf("platform node %s (%s): config.channel %q is not a valid channel handle (after casefold it must match %s)", n.ID, n.Compute, channel, platformChannelRE.String()),
			Path:     n.ID,
		}
	}
	return platformLeafPrefix + platform + "." + folded + ".last_" + event, nil
}

// platformStreamBindings synthesizes one
// ExternalAdapter{Kind:"platform-stream"} per DISTINCT platform leaf in
// the compiled node set (ADR 003 §3.3.3, normative). The binding is a
// PURE acceptance declaration: it exists so sceneAcceptsPath routes
// Quasar's scoped service-token writes to the scene — without it the
// write is silently absorbed. NO adapter goroutine is ever spawned for
// it (the poller / pg-listen starters filter on their own Kind), and
// none of the goroutine-bearing fields (URL, FrequencyHz, Channel) is
// set. Leaves are sorted for scene_version hash determinism.
func platformStreamBindings(nodes []GraphNode) []ExternalAdapter {
	seen := map[string]struct{}{}
	var leaves []string
	for _, n := range nodes {
		if _, _, ok := platformNodeRef(n.Compute); !ok {
			continue
		}
		if n.Path == "" {
			continue
		}
		if _, dup := seen[n.Path]; dup {
			continue
		}
		seen[n.Path] = struct{}{}
		leaves = append(leaves, n.Path)
	}
	sort.Strings(leaves)
	out := make([]ExternalAdapter, 0, len(leaves))
	for _, leaf := range leaves {
		out = append(out, ExternalAdapter{
			Key:         leaf,
			Label:       "Quasar platform stream",
			Kind:        "platform-stream",
			TargetPaths: []string{leaf},
		})
	}
	return out
}

// wiredPorts returns the set of input port names on nodeID that have an
// inbound edge (so their value comes from upstream, not a default).
func wiredPorts(nodeID string, edges []BlueprintEdge) map[string]struct{} {
	wired := make(map[string]struct{})
	for _, e := range edges {
		if e.ToNode == nodeID {
			wired[e.ToPort] = struct{}{}
		}
	}
	return wired
}

// topologicalSort returns the nodes in dependency order using Kahn's
// algorithm. A cycle in the blueprint edge graph yields an error.
func topologicalSort(nodes []GraphNode, edges []BlueprintEdge) ([]GraphNode, error) {
	inbound := make(map[string]int, len(nodes))
	byID := make(map[string]GraphNode, len(nodes))
	for _, n := range nodes {
		inbound[n.ID] = 0
		byID[n.ID] = n
	}
	out := make(map[string][]string, len(nodes))
	for _, e := range edges {
		// Skip edges referencing nodes the validator dropped.
		if _, ok := byID[e.FromNode]; !ok {
			continue
		}
		if _, ok := byID[e.ToNode]; !ok {
			continue
		}
		out[e.FromNode] = append(out[e.FromNode], e.ToNode)
		inbound[e.ToNode]++
	}

	var queue []string
	for id := range byID {
		if inbound[id] == 0 {
			queue = append(queue, id)
		}
	}
	// Stable order for hash determinism.
	sort.Strings(queue)

	var sorted []GraphNode
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, byID[id])
		var newReady []string
		for _, child := range out[id] {
			inbound[child]--
			if inbound[child] == 0 {
				newReady = append(newReady, child)
			}
		}
		sort.Strings(newReady)
		queue = append(queue, newReady...)
	}
	if len(sorted) != len(byID) {
		return nil, fmt.Errorf("topological sort: cycle detected (%d/%d nodes ordered)", len(sorted), len(byID))
	}
	return sorted, nil
}

// extractAdapters reads the layout's external_adapters declaration
// (Canvas authors them at scene-edit time, the compiler validates
// shape and forwards them to the bundle + graph).
//
// v1 stub: Canvas doesn't expose external_adapters in the layout
// type yet (waiting on chantier-canvas-extensions). The compiler is
// ready to read them; today we walk the bundle for any node whose
// kind starts with `adapter:` and pluck it out. This keeps the
// runtime contract intact while Canvas catches up.
func extractAdapters(layout *CanvasLayout, _ *[]OperatorInput, _ *Diagnostics) []ExternalAdapter {
	// Forward whatever the layout brought in. v1 layouts won't yet
	// carry adapter declarations; when Canvas ships its extensions,
	// CanvasLayout gets a typed field and this method matures.
	_ = layout
	return nil
}

// duplicateInputPaths returns the path values that appear more than once.
func duplicateInputPaths(inputs []OperatorInput) []string {
	seen := make(map[string]int, len(inputs))
	for _, in := range inputs {
		seen[in.Path]++
	}
	var dups []string
	for p, n := range seen {
		if n > 1 {
			dups = append(dups, p)
		}
	}
	sort.Strings(dups)
	return dups
}

// walkLayout yields every node in the tree rooted at n.
func walkLayout(n LayoutNode, fn func(LayoutNode)) {
	fn(n)
	for _, c := range n.Children {
		walkLayout(c, fn)
	}
}

// joinPath assembles a dotted instance path. Empty fragments collapse
// so root nodes don't carry a leading `.`.
func joinPath(parts ...string) string {
	var b []string
	for _, p := range parts {
		if p == "" {
			continue
		}
		b = append(b, p)
	}
	return strings.Join(b, ".")
}

// computeSceneVersion hashes both artefacts canonically. We use
// canonical JSON (keys sorted, no insignificant whitespace) so two
// pushes of identical inputs always land on the same hash.
func computeSceneVersion(graph *Graph, bundle *RenderBundle) (string, error) {
	g, err := canonicalJSON(graph)
	if err != nil {
		return "", err
	}
	b, err := canonicalJSON(bundle)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(g)
	h.Write([]byte("|"))
	h.Write(b)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalJSON marshals v with keys sorted recursively. encoding/json
// is already key-stable for maps but not for nested any-typed values;
// we round-trip via map[string]any to enforce sort everywhere.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return marshalCanonical(generic)
}

func marshalCanonical(v any) ([]byte, error) {
	buf := &bytes.Buffer{}
	if err := writeCanonical(buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		buf.WriteByte('{')
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(raw)
	}
	return nil
}
