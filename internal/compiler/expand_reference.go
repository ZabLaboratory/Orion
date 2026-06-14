package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// maxBlueprintRefExpansionDepth bounds recursive blueprint-reference
// expansion (ADR 014 §5 graph-explosion mitigation). It is a compiler
// constant, not an env var: it is a structural safety bound on the SHAPE of
// an authored graph, not an operator-tunable runtime parameter, and the cost
// is paid once at push (never per tick). A deep (acyclic) reference tree
// crosses it and is rejected with BLUEPRINT_REF_EXPANSION_LIMIT; a true cycle
// is caught first and more precisely by the dedicated detector
// (CYCLIC_BLUEPRINT_REFERENCE, issue #179) before this bound is reached.
const maxBlueprintRefExpansionDepth = 16

// expandReferences rewrites a blueprint graph so it contains NO `reference`
// nodes: each is replaced in-line by its pinned (blueprint_id, version)
// sub-graph, recursively, until the whole graph is flat `core.*` (ADR 014
// §3). The result feeds the UNCHANGED per-blueprint validation + topo-sort
// (compile.go), which only ever sees flat core.* nodes — exactly as a scene
// authored without any reference.
//
// It runs BEFORE manifest validation and the conformance gate, so the
// runtime never sees a `reference` (ADR 014 §3, decision 2a). Errors are
// returned as diagnostics (caller's HasErrors gate turns them into a
// POST /push failure, leaving latest_pushed_version unchanged).
//
// A blueprint with no reference node is returned unchanged (same pointer
// semantics as before): the loop simply finds nothing to expand, so a
// reference-free push is byte-identical to pre-ADR-014.
func expandReferences(ctx context.Context, b *BlueprintGraph, fetcher Fetcher) (*BlueprintGraph, []Diagnostic) {
	ex := &refExpander{
		fetcher: fetcher,
		// resolved memoises one fetch per (blueprint_id, version) for the
		// whole compile — the same memoisation discipline as the push-time
		// HTTP fetch (ADR 012 / http_fetcher), so a function referenced from
		// several sites is fetched once.
		resolved: map[string]*ResolvedBlueprintGraph{},
	}
	flat, diags := ex.expandGraph(ctx, b.Nodes, b.Edges, 0, nil)
	if len(diags) > 0 {
		return nil, diags
	}
	return &BlueprintGraph{ID: b.ID, Nodes: flat.nodes, Edges: flat.edges}, nil
}

type refExpander struct {
	fetcher  Fetcher
	resolved map[string]*ResolvedBlueprintGraph
	// site is a monotonically increasing counter giving every expansion
	// site a globally-unique alpha-rename prefix. A function referenced
	// twice (or recursively) yields two disjoint id namespaces, so no
	// inlined node id can ever collide with a sibling or with a node in the
	// parent graph (ADR 014 §3.2 — fresh activation record per call, like
	// Blue's _run_subgraph).
	site int
}

type flatGraph struct {
	nodes []BlueprintNode
	edges []BlueprintEdge
}

// expandGraph expands every reference node in (nodes, edges) into flat
// core.* and returns the merged result. depth bounds the recursion; stack is
// the (blueprint_id@version) resolution path — the chain of references on the
// CURRENT branch — against which expandOne detects cycles (A→A or A→B→A,
// CYCLIC_BLUEPRINT_REFERENCE, issue #179).
func (ex *refExpander) expandGraph(
	ctx context.Context,
	nodes []BlueprintNode,
	edges []BlueprintEdge,
	depth int,
	stack []string,
) (flatGraph, []Diagnostic) {
	var diags []Diagnostic
	out := flatGraph{
		nodes: make([]BlueprintNode, 0, len(nodes)),
		edges: make([]BlueprintEdge, 0, len(edges)),
	}

	// Partition the parent's nodes: plain nodes pass through verbatim;
	// reference nodes are expanded and their call-node edges rewired.
	refNodes := map[string]*BlueprintReference{}
	for i := range nodes {
		n := nodes[i]
		if n.Reference != nil {
			refNodes[n.ID] = n.Reference
			continue
		}
		out.nodes = append(out.nodes, n)
	}

	// Parent edges NOT touching a reference node pass through verbatim. An
	// edge into/out of a reference node is rewired onto the inlined
	// interface node during that reference's expansion (below), so we drop
	// it from the verbatim set and re-emit it remapped.
	for _, e := range edges {
		_, fromRef := refNodes[e.FromNode]
		_, toRef := refNodes[e.ToNode]
		if !fromRef && !toRef {
			out.edges = append(out.edges, e)
		}
	}

	// Expand each reference node. Iterate in stable id order so the
	// alpha-rename prefixes (and therefore the whole expanded graph) are
	// deterministic for a given input — the scene_version hash depends on it
	// (ADR 014 §3.6 / RC #7).
	refIDs := make([]string, 0, len(refNodes))
	for id := range refNodes {
		refIDs = append(refIDs, id)
	}
	sort.Strings(refIDs)

	for _, callID := range refIDs {
		ref := refNodes[callID]
		sub, refDiags := ex.expandOne(ctx, callID, ref, edges, depth, stack)
		if len(refDiags) > 0 {
			diags = append(diags, refDiags...)
			continue
		}
		out.nodes = append(out.nodes, sub.nodes...)
		out.edges = append(out.edges, sub.edges...)
	}

	return out, diags
}

// expandOne resolves and inlines a single reference node `callID`. parentEdges
// is the calling graph's full edge list (so the call node's in/out edges can
// be rewired onto the inlined interface nodes by port name).
func (ex *refExpander) expandOne(
	ctx context.Context,
	callID string,
	ref *BlueprintReference,
	parentEdges []BlueprintEdge,
	depth int,
	stack []string,
) (flatGraph, []Diagnostic) {
	if ref.BlueprintID == "" || ref.Version <= 0 {
		return flatGraph{}, []Diagnostic{{
			Code:     ErrBlueprintRefUnresolved,
			Severity: "error",
			Message: fmt.Sprintf(
				"reference node %s: missing or invalid (blueprint_id, version) pin", callID),
			Path: callID,
		}}
	}

	key := fmt.Sprintf("%s@%d", ref.BlueprintID, ref.Version)

	// Cycle detection (ADR 014 §5 / issue #179). If this key is already on the
	// CURRENT resolution path, inlining it loops forever — reject with the
	// precise CYCLIC_BLUEPRINT_REFERENCE, citing the offending chain. This is
	// the path STACK, not the memoised fetch set (ex.resolved): a function
	// legitimately reused across sibling branches of a DAG (diamond A→B, A→C,
	// B→D, C→D) appears in the fetch set twice but never twice on one path, so
	// it is NOT a cycle and expands normally.
	if onStack(stack, key) {
		return flatGraph{}, []Diagnostic{{
			Code:     ErrCyclicBlueprintReference,
			Severity: "error",
			Message: fmt.Sprintf(
				"reference node %s: cyclic blueprint reference: %s",
				callID, strings.Join(append(stack, key), " → ")),
			Path: callID,
		}}
	}

	// Depth/size bound (ADR 014 §5). With cycles caught above, crossing this
	// genuinely means the (acyclic) reference tree is too deep/wide; the
	// blow-up is rejected at push, not at runtime (cost paid once, at compile).
	if depth >= maxBlueprintRefExpansionDepth {
		return flatGraph{}, []Diagnostic{{
			Code:     ErrBlueprintRefExpansionLimit,
			Severity: "error",
			Message: fmt.Sprintf(
				"reference node %s: expansion limit (%d) reached resolving %s (stack: %v)",
				callID, maxBlueprintRefExpansionDepth, key, append(stack, key)),
			Path: callID,
		}}
	}

	resolved, diag := ex.resolve(ctx, callID, ref, key)
	if diag != nil {
		return flatGraph{}, []Diagnostic{*diag}
	}

	// Alpha-rename the whole sub-graph under a per-site unique prefix so no
	// inlined id collides with a sibling expansion or the parent graph.
	ex.site++
	prefix := fmt.Sprintf("__bpref%d__%s__", ex.site, callID)
	rename := func(id string) string { return prefix + id }

	// execTriggerable is true iff the resolved interface declares at least one
	// exec INPUT pin (kind == "exec"). It is read from the interface, never
	// guessed from a node/port name (graph-resolution.md § Exec pins / Orion
	// #186). When true, the caller's spine — spliced onto the inlined
	// `core.input@1`-exec — IS the trigger, so the function's own
	// `core.event.on-start@1` MUST be removed: keeping it would re-arm the
	// body at scene load (the ADR 003 §1.1 root cause) instead of on the
	// caller's trigger. When false (a data-only function), the on-start is
	// preserved verbatim — a data-only reference is byte-identical to
	// pre-#186 (anti-regression guard-rail, Vigil).
	execTriggerable := false
	for _, pin := range resolved.Interface.Inputs {
		if pin.Kind == execPinKind {
			execTriggerable = true
			break
		}
	}

	// The sub-graph's interface nodes (core.input@1 / core.output@1) are the
	// SPLICE points, not nodes that survive inlining. core.input@1 is a
	// leaf-bound source with no runtime executor: keeping it AND giving it an
	// inbound edge from the parent would reclassify it `computed`
	// (validateBlueprint) and the runtime would have nothing to run. So we
	// DROP every interface node and splice the parent's wiring directly onto
	// the interior nodes that referenced it — true flattening, byte-for-byte
	// what an inline-authored blueprint would look like (ADR 014 §3 / contract
	// "Port mapping"). Indexed by interface NAME (core.input@1 / core.output@1
	// carry it in config.name, the same key nodeLeafPath reads).
	inputNodeByID := map[string]string{}  // renamed core.input@1 id → interface name
	outputNodeByID := map[string]string{} // renamed core.output@1 id → interface name
	inputNames := map[string]struct{}{}   // declared interface input names present
	outputNames := map[string]struct{}{}  // declared interface output names present

	// droppedOnStart collects the renamed ids of `core.event.on-start@1` nodes
	// removed because the function is exec-triggerable: their edges must be
	// dropped too (below), so the parent never gets a stray spine that fires
	// at scene load. Empty for a data-only function (on-start preserved).
	droppedOnStart := map[string]struct{}{}

	sub := flatGraph{
		nodes: make([]BlueprintNode, 0, len(resolved.Nodes)),
		edges: make([]BlueprintEdge, 0, len(resolved.Edges)),
	}
	for _, n := range resolved.Nodes {
		rid := rename(n.ID)
		switch n.Compute {
		case coreInput:
			if name := interfaceNodeName(n); name != "" {
				inputNodeByID[rid] = name
				inputNames[name] = struct{}{}
				continue // dropped — spliced below
			}
		case coreOutput:
			if name := interfaceNodeName(n); name != "" {
				outputNodeByID[rid] = name
				outputNames[name] = struct{}{}
				continue // dropped — spliced below
			}
		case coreEventOnStart:
			// Drop the function's own on-start ONLY when it is
			// exec-triggerable: the caller's spine (arriving on the inlined
			// `exec_in` core.input@1) re-arms the interior spine in its place
			// (graph-resolution.md § Exec pins, Orion #186). A data-only
			// reference keeps its on-start verbatim — anti-regression.
			if execTriggerable {
				droppedOnStart[rid] = struct{}{}
				continue // dropped — its spine resumes from the exec_in splice
			}
		}
		// Interior node: keep verbatim under its renamed id. All interior
		// nodes are flat core.* already in Blue's manifest, so the existing
		// manifest gate stamps their purity (Blue derived it recursively, the
		// same source as resolved.Purity) — Orion never recomputes (ADR 014
		// §3.5 / ADR 006). resolved.Purity is a consistency assertion, not a
		// second source the compiler writes back.
		renamed := n
		renamed.ID = rid
		sub.nodes = append(sub.nodes, renamed)
	}

	// Build the splice maps from the parent's call-node edges:
	//   parentInput[name]  = the upstream {node, port} feeding call pin `name`
	//   parentOutputs[name]= the downstream {node, port} edges call pin `name`
	//                        feeds (a pin may fan out to several consumers)
	parentInput := map[string]struct{ node, port string }{}
	parentOutputs := map[string][]struct{ node, port string }{}
	for _, e := range parentEdges {
		if e.ToNode == callID {
			if _, ok := inputNames[e.ToPort]; !ok {
				return flatGraph{}, []Diagnostic{portMismatch(callID, key, "input", e.ToPort)}
			}
			parentInput[e.ToPort] = struct{ node, port string }{e.FromNode, e.FromPort}
		}
		if e.FromNode == callID {
			if _, ok := outputNames[e.FromPort]; !ok {
				return flatGraph{}, []Diagnostic{portMismatch(callID, key, "output", e.FromPort)}
			}
			parentOutputs[e.FromPort] = append(parentOutputs[e.FromPort],
				struct{ node, port string }{e.ToNode, e.ToPort})
		}
	}

	// Splice the sub-graph's internal edges across the dropped interface
	// nodes. An edge FROM a core.input@1 (name X) re-sources to the parent's
	// producer of pin X; an edge INTO a core.output@1 (name Y) re-targets to
	// every parent consumer of pin Y. An edge between two interior nodes
	// passes through with both ids renamed.
	for _, e := range resolved.Edges {
		// An edge incident on a dropped on-start (exec-triggerable function)
		// is removed with it: the interior spine it armed is re-armed by the
		// caller's spine via the exec_in splice instead. on-start is an
		// exec ENTRY (no inbound edges), so this only ever drops its outbound
		// `then` spine edge; guarding both ends is defensive and harmless.
		if _, drop := droppedOnStart[rename(e.FromNode)]; drop {
			continue
		}
		if _, drop := droppedOnStart[rename(e.ToNode)]; drop {
			continue
		}
		from, fromIsInput := inputNodeByID[rename(e.FromNode)]
		to, toIsOutput := outputNodeByID[rename(e.ToNode)]
		switch {
		case fromIsInput && toIsOutput:
			// A pass-through wire input→output: connect the parent producer of
			// the input pin straight to every parent consumer of the output pin.
			src, srcOK := parentInput[from]
			if !srcOK {
				continue // input pin unwired by the parent — nothing to deliver
			}
			for _, dst := range parentOutputs[to] {
				sub.edges = append(sub.edges, BlueprintEdge{
					FromNode: src.node, FromPort: src.port, ToNode: dst.node, ToPort: dst.port,
				})
			}
		case fromIsInput:
			src, srcOK := parentInput[from]
			if !srcOK {
				continue
			}
			sub.edges = append(sub.edges, BlueprintEdge{
				FromNode: src.node, FromPort: src.port,
				ToNode: rename(e.ToNode), ToPort: e.ToPort,
			})
		case toIsOutput:
			for _, dst := range parentOutputs[to] {
				sub.edges = append(sub.edges, BlueprintEdge{
					FromNode: rename(e.FromNode), FromPort: e.FromPort,
					ToNode: dst.node, ToPort: dst.port,
				})
			}
		default:
			sub.edges = append(sub.edges, BlueprintEdge{
				FromNode: rename(e.FromNode), FromPort: e.FromPort,
				ToNode: rename(e.ToNode), ToPort: e.ToPort,
			})
		}
	}

	// Recurse: the sub-graph may itself contain reference nodes (Blue serves
	// ONE level raw — Orion re-fetches each nested reference, contract "One
	// level only"). Push this key onto the resolution stack so a reference
	// back to it (on this path) is detected as a cycle by expandOne.
	nested, nestedDiags := ex.expandGraph(ctx, sub.nodes, sub.edges, depth+1, append(stack, key))
	if len(nestedDiags) > 0 {
		return flatGraph{}, nestedDiags
	}
	return nested, nil
}

// resolve fetches a pinned (blueprint_id, version) graph, memoised for the
// whole compile. A Blue typed 404/422 (wrapped ErrRefUnresolved) becomes a
// BLUEPRINT_REF_UNRESOLVED diagnostic; any other fetch error is a
// hard failure surfaced verbatim (transport / auth — FETCH_UPSTREAM class).
func (ex *refExpander) resolve(ctx context.Context, callID string, ref *BlueprintReference, key string) (*ResolvedBlueprintGraph, *Diagnostic) {
	if cached, ok := ex.resolved[key]; ok {
		return cached, nil
	}
	resolved, err := ex.fetcher.FetchBlueprintGraph(ctx, ref.BlueprintID, ref.Version)
	if err != nil {
		code := ErrFetchUpstream
		if errors.Is(err, ErrRefUnresolved) {
			code = ErrBlueprintRefUnresolved
		}
		return nil, &Diagnostic{
			Code:     code,
			Severity: "error",
			Message:  fmt.Sprintf("reference node %s (%s): %v", callID, key, err),
			Path:     callID,
		}
	}
	ex.resolved[key] = resolved
	return resolved, nil
}

// interfaceNodeName reads the interface name a core.input@1 / core.output@1
// declares — the same config.name key nodeLeafPath reads (ADR 004 §7.2,
// Blue stdlib_seeder). It is the join key against a reference node's port
// names during rewiring.
func interfaceNodeName(n BlueprintNode) string {
	raw, ok := n.Config["name"]
	if !ok {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return ""
	}
	return name
}

// portMismatch reports a reference node pin (dir = "input"|"output") with no
// matching core.input@1 / core.output@1 in the resolved interface — an
// authoring error (the call wires a port the function does not declare). It
// is a BLUEPRINT_REF_UNRESOLVED reject: the reference cannot be inlined.
func portMismatch(callID, key, dir, port string) Diagnostic {
	return Diagnostic{
		Code:     ErrBlueprintRefUnresolved,
		Severity: "error",
		Message: fmt.Sprintf(
			"reference node %s (%s): %s port %q has no matching core.%s@1 in the referenced interface",
			callID, key, dir, port, dir),
		Path: callID,
	}
}

func onStack(stack []string, key string) bool {
	for _, s := range stack {
		if s == key {
			return true
		}
	}
	return false
}
