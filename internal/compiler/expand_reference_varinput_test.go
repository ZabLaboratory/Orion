package compiler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Forge regression tests for the `__vars..` input-promotion bug (ADR 014):
// an exec-triggerable referenced function that stashes a db.query result in a
// blueprint-local `__vars..<var>` leaf (via core.variable.set@1) and reads it
// back through a `core.input@1` whose config.name IS that leaf path. The
// expander previously treated EVERY core.input@1 as an interface relay and
// dropped it — orphaning the reader's consumer (getScore lost its `record`
// input → walkPath(nil) → "" → placeholder "—" on air). The fix PROMOTES such
// a reader into the parent pure graph verbatim, re-namespaced per instance.
//
// These run directly against expandReferences so the assertion is on the flat
// graph, independent of the downstream partition/topo-sort.

const scoreRowsLeaf = varsLeafPrefix + ".score_rows" // "__vars..score_rows"

// scoreForPlayerGraph mirrors Blue's `score-for-player` exec-triggerable
// function (tests/fixtures/exec_triggerable_fns.py):
//
//	exec spine: exec_in → query(ranking) → setRows(variable.set score_rows) → then
//	data out:   score ← getScore(get-field path 0.score) ← inRows(core.input __vars..score_rows)
func scoreForPlayerGraph(id string, version int) *ResolvedBlueprintGraph {
	jcfg := func(v string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"variable": json.RawMessage(`"` + v + `"`)}
	}
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			inputNode("inMatchId", "match_id"),   // interface data pin
			inputNode("inPlayerId", "player_id"), // interface data pin
			{ID: "ein", Compute: coreInput,
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"exec_in"`)},
				Outputs: []BlueprintPort{execOut("then")}}, // exec interface pin
			{ID: "query", Compute: "core.db.query@1",
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("match_id"), dataIn("player_id")},
				Outputs: []BlueprintPort{execOut("then"), dataOut("rows")}},
			{ID: "setRows", Compute: coreVariableSet, Config: jcfg("score_rows"),
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
			// inRows is the bug epicentre: a core.input@1 whose config.name is
			// the `__vars..score_rows` LEAF — an internal reader, NOT an
			// interface pin. It must survive expansion (promoted) so getScore
			// keeps its `record`.
			{ID: "inRows", Compute: coreInput,
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"` + scoreRowsLeaf + `"`)},
				Outputs: []BlueprintPort{dataOut("value")}},
			{ID: "getScore", Compute: "core.data.get-field@1",
				Config:  map[string]json.RawMessage{"path": json.RawMessage(`"0.score"`)},
				Inputs:  []BlueprintPort{dataIn("record")},
				Outputs: []BlueprintPort{dataOut("value")}},
			outputNode("scoreOut", "score"), // interface data OUT pin (name != __vars)
		},
		Edges: []BlueprintEdge{
			{FromNode: "ein", FromPort: "then", ToNode: "query", ToPort: "exec_in"},
			{FromNode: "inMatchId", FromPort: "value", ToNode: "query", ToPort: "match_id"},
			{FromNode: "inPlayerId", FromPort: "value", ToNode: "query", ToPort: "player_id"},
			{FromNode: "query", FromPort: "then", ToNode: "setRows", ToPort: "exec_in"},
			{FromNode: "query", FromPort: "rows", ToNode: "setRows", ToPort: "value"},
			{FromNode: "inRows", FromPort: "value", ToNode: "getScore", ToPort: "record"},
			{FromNode: "getScore", FromPort: "value", ToNode: "scoreOut", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs: []BlueprintInterfacePin{
				{Name: "exec_in", Type: "exec", Kind: "exec", Required: true},
				{Name: "match_id", Type: "any", Kind: "data"},
				{Name: "player_id", Type: "any", Kind: "data"},
			},
			Outputs: []BlueprintInterfacePin{
				{Name: "then", Type: "exec", Kind: "exec"},
				{Name: "score", Type: "any", Kind: "data"},
			},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

func dataOut(name string) BlueprintPort { return BlueprintPort{Name: name, Type: "any", Kind: "data"} }

// callScoreFn authors one reference to score-for-player driven by on-start.
func callScoreFn(callID string) *BlueprintGraph {
	return &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
			refNode(callID, "bp-score", 1),
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: callID, ToPort: "exec_in"},
		},
	}
}

func scoreFetcher() *fakeFetcher {
	return &fakeFetcher{graphs: map[string]*ResolvedBlueprintGraph{
		"bp-score@1": scoreForPlayerGraph("bp-score", 1),
	}}
}

// nodeByCompute returns the single flat node of the given compute (fatal if not
// exactly one), for the single-instance assertions.
func nodeByCompute(t *testing.T, g *BlueprintGraph, compute string) BlueprintNode {
	t.Helper()
	var found []BlueprintNode
	for _, n := range g.Nodes {
		if n.Compute == compute {
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 %s node, got %d", compute, len(found))
	}
	return found[0]
}

func configString(t *testing.T, n BlueprintNode, key string) string {
	t.Helper()
	raw, ok := n.Config[key]
	if !ok {
		t.Fatalf("node %s missing config[%q]", n.ID, key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("node %s config[%q] not a string: %v", n.ID, key, err)
	}
	return s
}

// TestExpand_VarsInputReader_Promoted is RC #1/#4: after expanding a reference
// to score-for-player, getScore must keep a `record` input wired to a SURVIVING
// core.input@1 that reads the namespaced `__vars..*.score_rows` leaf — proving
// the internal reader was promoted, not dropped as an interface relay.
func TestExpand_VarsInputReader_Promoted(t *testing.T) {
	flat, diags := expandReferences(context.Background(), callScoreFn("L0sfp"), scoreFetcher())
	if len(diags) > 0 {
		t.Fatalf("expandReferences returned diagnostics: %+v", diags)
	}

	// The promoted reader survived: exactly one core.input@1 with a __vars.. name.
	getScore := nodeByCompute(t, flat, "core.data.get-field@1")

	var reader *BlueprintNode
	for i := range flat.Nodes {
		n := flat.Nodes[i]
		if n.Compute != coreInput {
			continue
		}
		name := configString(t, n, "name")
		if strings.HasPrefix(name, varsLeafPrefix) {
			if reader != nil {
				t.Fatalf("more than one promoted __vars reader: %s and %s", reader.ID, n.ID)
			}
			reader = &flat.Nodes[i]
		}
	}
	if reader == nil {
		t.Fatal("the `score_rows` core.input@1 reader was DROPPED (not promoted) — the bug")
	}

	// getScore.record is wired from the promoted reader (RC #1).
	wired := false
	for _, e := range flat.Edges {
		if e.ToNode == getScore.ID && e.ToPort == "record" && e.FromNode == reader.ID {
			wired = true
		}
	}
	if !wired {
		t.Fatalf("getScore (%s) has no `record` edge from the promoted reader (%s)\nedges=%+v",
			getScore.ID, reader.ID, flat.Edges)
	}

	// The set and the reader address the SAME instance leaf (set↔reader matched).
	setRows := nodeByCompute(t, flat, coreVariableSet)
	setVar := configString(t, setRows, "variable") // e.g. "bpref1_L0sfp_score_rows"
	readLeaf := configString(t, *reader, "name")   // "__vars..bpref1_L0sfp_score_rows"
	if readLeaf != varsLeafPrefix+"."+setVar {
		t.Fatalf("reader leaf %q does not match set var %q (set↔reader unmatched)", readLeaf, setVar)
	}
	if !strings.Contains(setVar, "score_rows") {
		t.Fatalf("namespaced var %q lost the original name", setVar)
	}
}

// TestExpand_VarsInputReader_DistinctPerInstance is the per-instance namespacing
// guard: two references to the same function must own DISTINCT `__vars` leaves,
// so 10 score-for-player calls do not clobber one shared `score_rows`.
func TestExpand_VarsInputReader_DistinctPerInstance(t *testing.T) {
	bp := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
			refNode("L0sfp", "bp-score", 1),
			refNode("R4sfp", "bp-score", 1),
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "L0sfp", ToPort: "exec_in"},
			{FromNode: "start", FromPort: "then", ToNode: "R4sfp", ToPort: "exec_in"},
		},
	}
	flat, diags := expandReferences(context.Background(), bp, scoreFetcher())
	if len(diags) > 0 {
		t.Fatalf("diagnostics: %+v", diags)
	}

	// Collect every promoted __vars reader leaf; there must be 2, and distinct.
	leaves := map[string]struct{}{}
	for _, n := range flat.Nodes {
		if n.Compute != coreInput {
			continue
		}
		name := configString(t, n, "name")
		if strings.HasPrefix(name, varsLeafPrefix) {
			leaves[name] = struct{}{}
		}
	}
	if len(leaves) != 2 {
		t.Fatalf("want 2 distinct promoted __vars reader leaves (one per instance), got %d: %v",
			len(leaves), leaves)
	}

	// And every variable.set writes a distinct leaf too (no clobber).
	setVars := map[string]struct{}{}
	for _, n := range flat.Nodes {
		if n.Compute == coreVariableSet {
			setVars[configString(t, n, "variable")] = struct{}{}
		}
	}
	if len(setVars) != 2 {
		t.Fatalf("want 2 distinct variable.set leaves (one per instance), got %d: %v",
			len(setVars), setVars)
	}
}

// TestExpand_InterfacePin_StillRelayed is the anti-regression: a TRUE interface
// core.input@1 (config.name is a port name, NOT a __vars leaf) must STILL be
// spliced away as a relay — the fix must not promote interface pins. After
// expansion, no core.input@1 named `match_id`/`player_id` may survive.
func TestExpand_InterfacePin_StillRelayed(t *testing.T) {
	flat, diags := expandReferences(context.Background(), callScoreFn("L0sfp"), scoreFetcher())
	if len(diags) > 0 {
		t.Fatalf("diagnostics: %+v", diags)
	}
	for _, n := range flat.Nodes {
		if n.Compute != coreInput {
			continue
		}
		name := configString(t, n, "name")
		if name == "match_id" || name == "player_id" || name == "exec_in" {
			t.Fatalf("interface pin core.input@1 %q survived expansion — should be a dropped relay", name)
		}
	}
}
