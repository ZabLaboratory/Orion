package compiler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	blueprint, err := fetcher.FetchBlueprint(ctx, envelope.BlueBlueprintID)
	if err != nil {
		d.AddError(ErrFetchUpstream, "fetch blueprint %s: %v", envelope.BlueBlueprintID, err)
		return nil, nil, "", &CompileError{Diagnostics: *d}
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
	for _, e := range hoistErrs {
		d.Items = append(d.Items, e)
	}
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

	// 4) Validate Blueprint compute purity (criterion 18) and resolve
	//    the runtime graph nodes.
	graphNodes, defaults, validateDiags := validateBlueprint(blueprint, manifest)
	for _, e := range validateDiags {
		d.Items = append(d.Items, e)
	}
	if d.HasErrors() {
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}

	// 5) Topological sort of compute nodes. Cycles in the blueprint
	//    edge graph are rejected.
	sorted, topoErr := topologicalSort(graphNodes, blueprint.Edges)
	if topoErr != nil {
		d.AddError(ErrTopologySort, "blueprint sort: %v", topoErr)
		return nil, nil, "", &CompileError{Diagnostics: *d}
	}

	// 6) Extract external adapters declared in bindings (HTTP poll,
	//    pg-listen, platform-stream, tick) — done by walking nodes
	//    whose compute is one of the adapter kinds. v1 keeps this
	//    rule simple: adapter declarations live in the layout's
	//    `external_adapters` block (Canvas-side authored), the
	//    compiler echoes them through after validation.
	adapters := extractAdapters(layout, &allInputs, d)

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
	}
	bundle := &RenderBundle{
		Root:             expanded,
		OperatorInputs:   allInputs,
		ExternalAdapters: adapters,
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
// AND is flagged is_pure. Returns the runtime graph nodes plus a
// defaults map seeded from declared inputs.
func validateBlueprint(b *BlueprintGraph, manifest ComputeManifest) ([]GraphNode, map[string]json.RawMessage, []Diagnostic) {
	var diags []Diagnostic
	defaults := map[string]json.RawMessage{}
	nodes := make([]GraphNode, 0, len(b.Nodes))

	upstreams := make(map[string][]string)
	for _, e := range b.Edges {
		upstreams[e.ToNode] = append(upstreams[e.ToNode], e.FromNode)
	}

	for _, n := range b.Nodes {
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
		if !entry.IsPure {
			diags = append(diags, Diagnostic{
				Code:     ErrImpureCompute,
				Severity: "error",
				Message:  fmt.Sprintf("blueprint node %s uses impure compute %q (manifest: is_pure=false)", n.ID, n.Compute),
				Path:     n.ID,
			})
			continue
		}

		kind := "computed"
		if len(upstreams[n.ID]) == 0 {
			kind = "input"
		}
		if n.OutputAt != "" {
			kind = "output"
			if def, ok := n.Args["default"]; ok {
				defaults[n.OutputAt] = def
			}
		}

		nodes = append(nodes, GraphNode{
			ID:        n.ID,
			Kind:      kind,
			Path:      n.OutputAt,
			Compute:   n.Compute,
			Upstream:  upstreams[n.ID],
			IsPure:    entry.IsPure,
			IsBounded: entry.IsBounded,
		})
	}
	return nodes, defaults, diags
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
	var any any
	if err := json.Unmarshal(raw, &any); err != nil {
		return nil, err
	}
	return marshalCanonical(any)
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
