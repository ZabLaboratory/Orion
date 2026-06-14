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

// relayInterfaceTag marks a renamed interface relay node (core.input@1 /
// core.output@1) carried THROUGH expansion and bypassed-then-dropped in a
// final pass. Keeping the relays alive during recursion lets the bypass pass
// reconnect splice points uniformly — including an edge BETWEEN two reference
// nodes (a sequential exec chain `refA.then → refB.exec_in`) and a chain of
// pass-through wires across several references — which a per-reference splice
// could not, because each reference saw the other endpoint only as a sibling
// CALL node id, not as its inlined interior (the missing-inter-reference-edge
// bug: 9 of the 10 `score-for-player` references left orphaned, ADR 003 §3.C).

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
		relays:   map[string]struct{}{},
	}
	flat, diags := ex.expandGraph(ctx, b.Nodes, b.Edges, 0, nil)
	if len(diags) > 0 {
		return nil, diags
	}
	// Final bypass: every interface relay carried through expansion is removed
	// here, its producers wired straight to its consumers. Doing it ONCE over
	// the fully-inlined graph reconnects splice points across reference
	// boundaries — adjacent references and pass-through chains alike — which a
	// per-reference splice cannot (each reference sees its neighbour only as a
	// CALL node, not as inlined interior).
	flat = ex.dropRelays(flat)
	// Surface the per-instance `__vars..` constant seeds harvested from each
	// inlined reference's `variables[].value` (Orion #192). They ride out on
	// BlueprintGraph.Defaults (an output-only carrier) for the per-blueprint
	// compile loop to fold into graph.Defaults — the only path by which a
	// referenced function's declared constant (e.g. a colour palette read by
	// `core.variable.get@1`) reaches the runtime. Nil when no reference
	// declared a valued variable — byte-identical to pre-#192.
	return &BlueprintGraph{ID: b.ID, Nodes: flat.nodes, Edges: flat.edges, Defaults: flat.defaults}, nil
}

type refExpander struct {
	fetcher  Fetcher
	resolved map[string]*ResolvedBlueprintGraph
	// relays is the set of renamed interface relay node ids carried through
	// expansion. Populated during inlining, consumed (bypassed + removed) by
	// dropRelays at the end (ADR 014 §3 splice). Direction is irrelevant to the
	// bypass — a relay is bypassed identically whether input or output side.
	relays map[string]struct{}
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
	// defaults accumulates the per-instance `__vars..<varNS(name)>` → value
	// seeds from inlined references' `variables[].value` (Orion #192). It is
	// merged bottom-up (expandOne → expandGraph) and carried verbatim through
	// dropRelays. Keys are in the pre-blueprint-key (empty-key) `__vars..` form
	// — prefixDefaultLeaf substitutes the real key per blueprint at fold time,
	// the same rewrite prefixGraphNodes applies to the reading node's leaf.
	defaults map[string]json.RawMessage
}

// mergeDefaults folds src into dst (allocating dst on first use), returning the
// (possibly new) map. Used to lift each reference's variable seeds up the
// expansion tree. A nil/empty src is a no-op (dst returned unchanged).
func mergeDefaults(dst, src map[string]json.RawMessage) map[string]json.RawMessage {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]json.RawMessage, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// endpoint is a concrete (node, port) address in the flattened graph — the
// non-relay producer dropRelays splices a relay's consumers onto.
type endpoint struct {
	node string
	port string
}

// expandGraph expands every reference node in (nodes, edges) into flat
// core.* (plus the interface relays dropRelays removes at the end) and returns
// the merged result. depth bounds the recursion; stack is the
// (blueprint_id@version) resolution path — the chain of references on the
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

	// Partition the parent's nodes: plain nodes pass through verbatim; each
	// reference node is replaced by its inlined sub-graph (interior nodes plus
	// renamed interface relays), and its call-node edges are rewired onto those
	// relays (below).
	refNodes := map[string]*BlueprintReference{}
	for i := range nodes {
		n := nodes[i]
		if n.Reference != nil {
			refNodes[n.ID] = n.Reference
			continue
		}
		out.nodes = append(out.nodes, n)
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

	// relayByCall[callID][pinName] = the renamed relay node id for that
	// reference's interface pin, so the parent call-node edges can be rewired
	// onto it after every sibling has produced its relays.
	relayByCall := map[string]map[string]string{}
	pinNames := map[string]struct{ in, out map[string]struct{} }{}
	for _, callID := range refIDs {
		ref := refNodes[callID]
		sub, relayPins, names, refDiags := ex.expandOne(ctx, callID, ref, depth, stack)
		if len(refDiags) > 0 {
			diags = append(diags, refDiags...)
			continue
		}
		out.nodes = append(out.nodes, sub.nodes...)
		out.edges = append(out.edges, sub.edges...)
		// Lift this reference's `__vars..` constant seeds (its own variables +
		// any from references it nested) into the parent graph (Orion #192).
		out.defaults = mergeDefaults(out.defaults, sub.defaults)
		relayByCall[callID] = relayPins
		pinNames[callID] = names
	}
	if len(diags) > 0 {
		return out, diags
	}

	// Rewire every parent edge that touches a reference call node onto the
	// reference's interface relay. An edge into call.pin re-targets to the
	// input relay; an edge out of call.pin re-sources from the output relay.
	// When BOTH endpoints are references (the sequential exec chain
	// `refA.then → refB.exec_in`), each side resolves to its own relay, so the
	// edge survives as relay→relay and dropRelays bypasses both — the fix for
	// the orphaned references. A non-ref endpoint stays verbatim.
	for _, e := range edges {
		_, fromRef := refNodes[e.FromNode]
		_, toRef := refNodes[e.ToNode]
		if !fromRef && !toRef {
			out.edges = append(out.edges, e) // verbatim — touches no reference
			continue
		}
		ne := e
		if fromRef {
			if _, ok := pinNames[e.FromNode].out[e.FromPort]; !ok {
				diags = append(diags, portMismatch(e.FromNode, refKey(refNodes[e.FromNode]), "output", e.FromPort))
				continue
			}
			ne.FromNode = relayByCall[e.FromNode][e.FromPort]
			ne.FromPort = relayValuePort
		}
		if toRef {
			if _, ok := pinNames[e.ToNode].in[e.ToPort]; !ok {
				diags = append(diags, portMismatch(e.ToNode, refKey(refNodes[e.ToNode]), "input", e.ToPort))
				continue
			}
			ne.ToNode = relayByCall[e.ToNode][e.ToPort]
			ne.ToPort = relayValuePort
		}
		out.edges = append(out.edges, ne)
	}

	return out, diags
}

// relayValuePort is the single synthetic port every interface relay carries on
// both ends. Relays never reach validation (dropRelays removes them), so the
// port name only has to be internally consistent between the rewired parent
// edge and the body edge incident on the relay.
const relayValuePort = "__relay"

// refKey rebuilds the (blueprint_id@version) key for a diagnostic message.
func refKey(ref *BlueprintReference) string {
	if ref == nil {
		return ""
	}
	return fmt.Sprintf("%s@%d", ref.BlueprintID, ref.Version)
}

// expandOne resolves and inlines a single reference node `callID`, returning:
//   - the inlined sub-graph: interior nodes + relay nodes + interior/body edges
//     (every body edge incident on an interface node is re-pointed at that
//     interface's relay node, carrying relayValuePort);
//   - relayPins[pinName] = the renamed relay node id for each interface pin, so
//     expandGraph can rewire the parent call-node edges onto it;
//   - names = the declared interface input/output pin names, for the parent-edge
//     port-mismatch check.
//
// Interface nodes are kept as RELAY nodes (not spliced here) so the single
// final dropRelays pass can bypass them across reference boundaries.
func (ex *refExpander) expandOne(
	ctx context.Context,
	callID string,
	ref *BlueprintReference,
	depth int,
	stack []string,
) (flatGraph, map[string]string, struct{ in, out map[string]struct{} }, []Diagnostic) {
	noNames := struct{ in, out map[string]struct{} }{}
	if ref.BlueprintID == "" || ref.Version <= 0 {
		return flatGraph{}, nil, noNames, []Diagnostic{{
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
		return flatGraph{}, nil, noNames, []Diagnostic{{
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
		return flatGraph{}, nil, noNames, []Diagnostic{{
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
		return flatGraph{}, nil, noNames, []Diagnostic{*diag}
	}

	// Alpha-rename the whole sub-graph under a per-site unique prefix so no
	// inlined id collides with a sibling expansion or the parent graph.
	ex.site++
	prefix := fmt.Sprintf("__bpref%d__%s__", ex.site, callID)
	rename := func(id string) string { return prefix + id }

	// varNS namespaces a function-local variable name per reference INSTANCE,
	// the data-space analogue of the per-site node-id `prefix`. A function body
	// holds blueprint-local state in `__vars..<var>` leaves written by
	// `core.variable.set@1`/read by `core.variable.get@1` and (the bug below)
	// by `core.input@1` nodes whose config.name is the `__vars..<var>` leaf
	// path. Those leaves are addressed by NAME, not node id, so the per-site id
	// `prefix` does NOT keep two instances of the same function apart: 10
	// `score-for-player` references would all write/read one shared
	// `__vars..score_rows`, the last db.query clobbering the rest. Namespacing
	// the bare var name per instance (set, get AND the input-reader leaf all
	// rewritten the same way) gives each instance its own leaf while keeping its
	// own set↔get↔reader matched. The final `__vars.<BlueprintKey>.` prefixing
	// (prefixGraphNodes) wraps this unchanged. A scene-level (non-reference)
	// blueprint never enters expandOne, so its `roster_rows` is untouched.
	varNS := func(v string) string { return fmt.Sprintf("bpref%d_%s_%s", ex.site, callID, v) }

	// execTriggerable is true iff the resolved interface declares at least one
	// exec INPUT pin (kind == "exec"). It is read from the interface, never
	// guessed from a node/port name (graph-resolution.md § Exec pins / Orion
	// #186). When true, the caller's spine — spliced onto the inlined
	// `exec_in` relay — IS the trigger, so the function's own
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

	// Interface nodes (core.input@1 / core.output@1) are kept as RELAY nodes,
	// indexed by interface NAME (carried in config.name, the same key
	// nodeLeafPath reads). dropRelays bypasses and removes them at the end.
	relayPins := map[string]string{}      // interface name → renamed relay node id
	interfaceRelay := map[string]string{} // renamed interface node id → relay node id
	inNames := map[string]struct{}{}      // declared interface input names present
	outNames := map[string]struct{}{}     // declared interface output names present

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
			// A core.input@1 is one of TWO distinct things, told apart by its
			// config.name (graph-resolution.md § core.input). When the name is
			// a `__vars..<var>` LEAF path it is NOT an interface pin: it is an
			// internal reader of state a sibling `variable.set` wrote in the
			// SAME body (the `score-for-player` shape — db.query → set score_rows,
			// then getScore reads `core.input(__vars..score_rows)`). Such a node
			// must be PROMOTED into the parent pure graph verbatim (renamed id,
			// its leaf re-namespaced per instance), never turned into an
			// interface relay and dropped — dropping it orphaned `getScore`
			// (no `record` input → walkPath(nil) → "" → placeholder "—", ADR 014).
			// Only a name that is NOT a `__vars..` leaf is a true interface pin.
			if name := interfaceNodeName(n); name != "" && !strings.HasPrefix(name, varsLeafPrefix) {
				relayID := rid
				relayPins[name] = relayID
				interfaceRelay[rid] = relayID
				inNames[name] = struct{}{}
				ex.relays[relayID] = struct{}{}
				// Keep the node as an opaque relay placeholder — dropRelays
				// removes it; it never reaches validation. Recorded above.
				sub.nodes = append(sub.nodes, BlueprintNode{ID: relayID})
				continue
			}
			// Falls through to the interior-node tail: a `__vars..` reader is
			// kept verbatim, its leaf re-namespaced per instance by namespaceVarsConfig.
		case coreOutput:
			if name := interfaceNodeName(n); name != "" {
				relayID := rid
				relayPins[name] = relayID
				interfaceRelay[rid] = relayID
				outNames[name] = struct{}{}
				ex.relays[relayID] = struct{}{}
				sub.nodes = append(sub.nodes, BlueprintNode{ID: relayID})
				continue
			}
		case coreEventOnStart:
			// Drop the function's own on-start ONLY when it is
			// exec-triggerable: the caller's spine (arriving on the inlined
			// `exec_in` relay) re-arms the interior spine in its place
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
		// Per-instance variable namespacing (see varNS): rewrite the function-
		// local var name on every node that addresses `__vars` state so each
		// reference instance owns a distinct leaf. `variable.set`/`variable.get`
		// carry it in config.variable; a promoted `core.input@1` reader carries
		// the whole `__vars..<var>` leaf in config.name. Matched rewrites keep
		// each instance's set↔get↔reader pointed at the SAME instance leaf.
		renamed.Config = namespaceVarsConfig(n.Compute, n.Config, varNS)
		sub.nodes = append(sub.nodes, renamed)
	}

	// Carry the body edges, re-pointing each end incident on an interface node
	// at that interface's relay (so dropRelays sees one consistent relay port).
	// Edges incident on a dropped on-start are removed with it.
	for _, e := range resolved.Edges {
		if _, drop := droppedOnStart[rename(e.FromNode)]; drop {
			continue
		}
		if _, drop := droppedOnStart[rename(e.ToNode)]; drop {
			continue
		}
		ne := BlueprintEdge{
			FromNode: rename(e.FromNode), FromPort: e.FromPort,
			ToNode: rename(e.ToNode), ToPort: e.ToPort,
		}
		if relay, ok := interfaceRelay[ne.FromNode]; ok {
			ne.FromNode, ne.FromPort = relay, relayValuePort
		}
		if relay, ok := interfaceRelay[ne.ToNode]; ok {
			ne.ToNode, ne.ToPort = relay, relayValuePort
		}
		sub.edges = append(sub.edges, ne)
	}

	// Harvest this reference's declared constants (Orion #192). A
	// `variables[].value` is a blueprint-local CONSTANT the body reads through
	// a `core.variable.get@1 {variable: <name>}` whose leaf is `__vars..<name>`
	// (nodeLeafPath). Nothing wires that leaf, so the value must arrive as a
	// graph default. The reader's `config.variable` is namespaced per instance
	// (namespaceVarsConfig → varNS) above, so the seed leaf MUST use the SAME
	// per-instance var name: varsLeaf(varNS(name)) === the leaf the namespaced
	// `core.variable.get@1` resolves. A Value-less variable (pure shared state,
	// written by a `variable.set` before any read) carries no constant — skip.
	for _, v := range resolved.Variables {
		if len(v.Value) == 0 || v.Name == "" {
			continue
		}
		if sub.defaults == nil {
			sub.defaults = map[string]json.RawMessage{}
		}
		sub.defaults[varsLeaf(varNS(v.Name))] = v.Value
	}

	// Recurse: the sub-graph may itself contain reference nodes (Blue serves
	// ONE level raw — Orion re-fetches each nested reference, contract "One
	// level only"). Push this key onto the resolution stack so a reference
	// back to it (on this path) is detected as a cycle by expandOne. The
	// recursion only inlines nested references; relays of THIS reference pass
	// through untouched (they are plain nodes to the nested pass).
	nested, nestedDiags := ex.expandGraph(ctx, sub.nodes, sub.edges, depth+1, append(stack, key))
	if len(nestedDiags) > 0 {
		return flatGraph{}, nil, noNames, nestedDiags
	}
	// The nested pass carries any seeds from references THIS body nested; fold
	// in this reference's own variable seeds so the whole subtree's constants
	// ride out together (Orion #192).
	nested.defaults = mergeDefaults(nested.defaults, sub.defaults)
	names := struct{ in, out map[string]struct{} }{in: inNames, out: outNames}
	return nested, relayPins, names, nil
}

// dropRelays removes every interface relay node, splicing each relay's
// producers straight to its consumers. It iterates to a fixpoint so a chain of
// relays (relay→relay, produced by a pass-through wire spanning several
// references, or by two adjacent references on one spine) collapses to direct
// producer→consumer edges. Relays carry no executor and never reach
// validation; this is the true flattening (ADR 014 §3 "Port mapping").
func (ex *refExpander) dropRelays(g flatGraph) flatGraph {
	if len(ex.relays) == 0 {
		return g
	}

	// Index the edges entering each relay (its producer side). A producer may
	// itself be a relay (a relay→relay link: an inter-reference spine edge or a
	// pass-through wire), which is why producers are resolved TRANSITIVELY below.
	inEdges := map[string][]BlueprintEdge{}
	for _, e := range g.edges {
		if isRelay(ex.relays, e.ToNode) {
			inEdges[e.ToNode] = append(inEdges[e.ToNode], e)
		}
	}

	// resolveProducers returns the concrete (non-relay) producer endpoints
	// feeding `relayID`, chasing relay→relay links to their non-relay source.
	// Memoised; `onPath` guards the (already cycle-checked) graph defensively so
	// a pathological relay loop cannot spin forever here.
	memo := map[string][]endpoint{}
	var resolveProducers func(relayID string, onPath map[string]bool) []endpoint
	resolveProducers = func(relayID string, onPath map[string]bool) []endpoint {
		if cached, ok := memo[relayID]; ok {
			return cached
		}
		if onPath[relayID] {
			return nil // defensive: relay cycle (should never happen post-#179)
		}
		onPath[relayID] = true
		var out []endpoint
		for _, e := range inEdges[relayID] {
			if isRelay(ex.relays, e.FromNode) {
				out = append(out, resolveProducers(e.FromNode, onPath)...)
			} else {
				out = append(out, endpoint{node: e.FromNode, port: e.FromPort})
			}
		}
		delete(onPath, relayID)
		memo[relayID] = out
		return out
	}

	// Emit the flattened edges. A verbatim edge (neither end a relay) passes
	// through. An edge OUT of a relay to a NON-relay consumer is the splice
	// point: connect every resolved concrete producer of that relay straight to
	// the consumer. Edges into a relay, and relay→relay edges, are not emitted
	// directly — they are folded into resolveProducers. The result is true
	// flattening: producer→consumer with no relay left (ADR 014 §3 "Port
	// mapping"); an unwired pin (relay with no producers) simply delivers
	// nothing, which is correct.
	cleaned := make([]BlueprintEdge, 0, len(g.edges))
	for _, e := range g.edges {
		fromRelay := isRelay(ex.relays, e.FromNode)
		toRelay := isRelay(ex.relays, e.ToNode)
		switch {
		case !fromRelay && !toRelay:
			cleaned = append(cleaned, e)
		case fromRelay && !toRelay:
			for _, p := range resolveProducers(e.FromNode, map[string]bool{}) {
				cleaned = append(cleaned, BlueprintEdge{
					FromNode: p.node, FromPort: p.port,
					ToNode: e.ToNode, ToPort: e.ToPort,
				})
			}
		default:
			// into a relay, or relay→relay — folded into resolveProducers.
		}
	}

	// Strip the relay placeholder nodes — none survives to validation.
	nodes := make([]BlueprintNode, 0, len(g.nodes))
	for _, n := range g.nodes {
		if isRelay(ex.relays, n.ID) {
			continue
		}
		nodes = append(nodes, n)
	}
	// Carry the harvested `__vars..` constant seeds through verbatim — relay
	// removal never touches them (Orion #192).
	return flatGraph{nodes: nodes, edges: cleaned, defaults: g.defaults}
}

func isRelay(relays map[string]struct{}, id string) bool {
	_, ok := relays[id]
	return ok
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

// coreVariableSet is the writer half of a function's blueprint-local state
// (its reader is coreVariableGet, compile.go). Both carry the variable name in
// config.variable; the reference expander namespaces that name per instance
// (namespaceVarsConfig) so two inlinings of one function own disjoint leaves.
const coreVariableSet = "core.variable.set@1"

// namespaceVarsConfig returns a copy of `cfg` with the node's function-local
// variable name rewritten through ns, for the THREE node shapes that address
// `__vars` state inside a referenced body:
//
//   - core.variable.set@1 / core.variable.get@1: config.variable = "<var>"
//     → "<ns(var)>".
//   - core.input@1 reading the leaf: config.name = "__vars..<var>"
//     → "__vars..<ns(var)>".
//
// Rewriting all three identically keeps one instance's set↔get↔reader matched
// while distinguishing instances (varNS). Any other node, or a config the node
// does not own, is returned unchanged (a shallow copy — the original config map
// must not be mutated, it is shared with the memoised resolved graph). A
// data-only function with no `__vars` state is byte-identical to before.
func namespaceVarsConfig(compute string, cfg map[string]json.RawMessage, ns func(string) string) map[string]json.RawMessage {
	if len(cfg) == 0 {
		return cfg
	}
	var rewriteKey string
	var transform func(string) string
	switch compute {
	case coreVariableSet, coreVariableGet:
		rewriteKey, transform = "variable", ns
	case coreInput:
		name := interfaceNodeName(BlueprintNode{Config: cfg})
		if !strings.HasPrefix(name, varsLeafPrefix) {
			return cfg // a true interface pin — no var leaf to namespace
		}
		// "__vars..<var>" → "__vars..<ns(var)>": keep the prefix, namespace the
		// var segment after the empty-key double dot (varsLeaf("") == "__vars..").
		rewriteKey, transform = "name", func(leaf string) string {
			return varsLeaf(ns(leaf[len(varsLeaf("")):]))
		}
	default:
		return cfg
	}

	raw, ok := cfg[rewriteKey]
	if !ok {
		return cfg
	}
	var cur string
	if err := json.Unmarshal(raw, &cur); err != nil || cur == "" {
		return cfg // malformed/empty — leave it for the downstream gate to reject
	}
	out := make(map[string]json.RawMessage, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	encoded, err := json.Marshal(transform(cur))
	if err != nil {
		return cfg
	}
	out[rewriteKey] = encoded
	return out
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
