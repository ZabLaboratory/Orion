package compiler

import (
	"context"
	"strings"
	"testing"
)

// Forge regression tests for the sequential-reference exec-chain bug
// (ADR 003 §3.C, issue #100 / Orion #186): when N exec-triggerable references
// are chained on one spine (`refK.then → ref(K+1).exec_in`), the splice must
// reconnect EVERY inter-reference edge, not just the first. The original
// expander spliced each reference against the parent's CALL-node edges in
// isolation, so an edge whose other end was a SIBLING reference call node
// (dropped during its own expansion) was lost: only the first reference's body
// stayed reachable, the other N-1 were orphaned (the live symptom: 9 of 10
// `score-for-player` references never executed, `pl.*` stuck at placeholder).
//
// These run directly against expandReferences so the assertion is on the flat
// graph edges/nodes, independent of the downstream partition/topo-sort.

// seqLink is a minimal exec-triggerable referenced function: an `exec_in` relay
// drives a single interior `core.db.query@1`, whose own `then` is the function's
// `then` output. Two of these chained must keep the second's query reachable.
//
//	exec_in (core.input@1, kind:exec) --then--> q (db.query) --then--> then (core.output@1, kind:exec)
func seqLink(id string, version int) *ResolvedBlueprintGraph {
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			inputNode("ein", "exec_in"),
			{ID: "q", Compute: "core.db.query@1",
				Inputs:  []BlueprintPort{execIn("exec_in")},
				Outputs: []BlueprintPort{execOut("then")}},
			outputNode("eout", "then"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "ein", FromPort: "then", ToNode: "q", ToPort: "exec_in"},
			{FromNode: "q", FromPort: "then", ToNode: "eout", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs:  []BlueprintInterfacePin{{Name: "exec_in", Type: "exec", Kind: "exec", Required: true}},
			Outputs: []BlueprintInterfacePin{{Name: "then", Type: "exec", Kind: "exec"}},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

// fanFetch builds a fetcher serving the chained links plus the manifest.
func seqFetcher(graphs map[string]*ResolvedBlueprintGraph) *fakeFetcher {
	return &fakeFetcher{graphs: graphs}
}

// adjacentChainBP authors `start → sfp0 → sfp1 → ... → sfp(n-1)` on one spine:
// start.then → sfp0.exec_in, then each sfp(K).then → sfp(K+1).exec_in. The last
// `then` is left unwired (a terminal effect chain), exactly the databound shape.
func adjacentChainBP(n int) *BlueprintGraph {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
		},
	}
	prev := struct{ node, port string }{"start", "then"}
	for k := 0; k < n; k++ {
		id := sfpID(k)
		bp.Nodes = append(bp.Nodes, refNode(id, "bp-link", 1))
		bp.Edges = append(bp.Edges, BlueprintEdge{
			FromNode: prev.node, FromPort: prev.port, ToNode: id, ToPort: "exec_in",
		})
		prev = struct{ node, port string }{id, "then"}
	}
	return bp
}

func sfpID(k int) string {
	return "sfp" + string(rune('0'+k))
}

// TestExpand_AdjacentReferences_AllReachable is the direct regression: 10
// adjacent exec references must ALL inline a reachable db.query, none orphaned.
func TestExpand_AdjacentReferences_AllReachable(t *testing.T) {
	const n = 10
	bp := adjacentChainBP(n)
	f := seqFetcher(map[string]*ResolvedBlueprintGraph{
		"bp-link@1": seqLink("bp-link", 1),
	})

	flat, diags := expandReferences(context.Background(), bp, f)
	if len(diags) > 0 {
		t.Fatalf("expandReferences returned diagnostics: %+v", diags)
	}

	// No reference node leaked, and no interface relay survived.
	for _, nn := range flat.Nodes {
		if nn.Reference != nil {
			t.Fatalf("reference node %s leaked into the flat graph", nn.ID)
		}
		if strings.HasSuffix(nn.ID, "__relay") || nn.ID == "" {
			t.Fatalf("interface relay node %q survived flattening", nn.ID)
		}
	}

	// Each link inlines exactly one db.query — expect n of them.
	queries := make([]string, 0, n)
	for _, nn := range flat.Nodes {
		if nn.Compute == "core.db.query@1" {
			queries = append(queries, nn.ID)
		}
	}
	if len(queries) != n {
		t.Fatalf("want %d inlined db.query nodes, got %d: %v", n, len(queries), queries)
	}

	// Reachability invariant: every exec node is reachable from the entrypoint
	// `start` over exec edges. This is the guard-rail that would have caught
	// the 18 orphaned nodes. We walk all edges (exec splice carries through the
	// flattened graph) from `start`; every db.query must be visited.
	reached := reachableFrom(flat, "start")
	var orphans []string
	for _, q := range queries {
		if !reached[q] {
			orphans = append(orphans, q)
		}
	}
	if len(orphans) > 0 {
		t.Fatalf("UNREACHABLE inlined db.query nodes (orphaned references): %v\nreached=%v", orphans, reached)
	}
}

// TestExpand_AdjacentReferences_ContiguousSpine asserts the inter-reference
// edges are exactly the spliced query→query chain: each link's db.query feeds
// the next link's db.query on the exec spine (no broken `then` cul-de-sac).
func TestExpand_AdjacentReferences_ContiguousSpine(t *testing.T) {
	const n = 5
	bp := adjacentChainBP(n)
	f := seqFetcher(map[string]*ResolvedBlueprintGraph{"bp-link@1": seqLink("bp-link", 1)})

	flat, diags := expandReferences(context.Background(), bp, f)
	if len(diags) > 0 {
		t.Fatalf("diagnostics: %+v", diags)
	}

	// Build node id → compute, and an exec adjacency over the flat edges.
	compute := map[string]string{}
	for _, nn := range flat.Nodes {
		compute[nn.ID] = nn.Compute
	}

	// Count edges whose BOTH ends are inlined db.query nodes (the spliced
	// inter-reference links). For n chained links there must be n-1 of them.
	q2q := 0
	for _, e := range flat.Edges {
		if compute[e.FromNode] == "core.db.query@1" && compute[e.ToNode] == "core.db.query@1" {
			q2q++
		}
	}
	if q2q != n-1 {
		t.Fatalf("want %d query→query spliced links (contiguous spine), got %d\nedges=%+v",
			n-1, q2q, flat.Edges)
	}

	// And the caller's `start` reaches the FIRST link's query (spine origin).
	startTargets := 0
	for _, e := range flat.Edges {
		if e.FromNode == "start" && compute[e.ToNode] == "core.db.query@1" {
			startTargets++
		}
	}
	if startTargets != 1 {
		t.Fatalf("want start.then spliced onto exactly 1 db.query (chain origin), got %d", startTargets)
	}
}

// reachableFrom returns the set of node ids reachable from root over ALL edges
// (data and exec) in the flat graph — a conservative over-approximation of exec
// reachability that is sufficient for the orphan check: an orphaned reference
// body has NO inbound edge at all, so it stays unreached under any walk.
func reachableFrom(g *BlueprintGraph, root string) map[string]bool {
	adj := map[string][]string{}
	for _, e := range g.Edges {
		adj[e.FromNode] = append(adj[e.FromNode], e.ToNode)
	}
	seen := map[string]bool{root: true}
	stack := []string{root}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, nxt := range adj[cur] {
			if !seen[nxt] {
				seen[nxt] = true
				stack = append(stack, nxt)
			}
		}
	}
	return seen
}
