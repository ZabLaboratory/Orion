package compiler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Probe coverage for the ADR 014 __vars input-promotion fix.
// Forge's tests (expand_reference_varinput_test.go) prove the happy-path
// for a single reference: promotion, distinct-per-instance namespacing, and
// interface-pin relay still dropped. These tests probe the BLIND SPOTS:
//
//  1. Nested reference: two levels of reference, each with its own
//     variable.set + core.input@1 reader on a __vars leaf. No double-prefix,
//     no collision, set↔reader matched at each level.
//  2. variable.get inside a reference: coherent namespacing with variable.set.
//  3. Author-name collision: two distinct blueprint functions sharing the same
//     internal variable name (score_rows) must not clobber each other.
//
// The runtime end-to-end proof (RC #2 value, not just address) lives in
// internal/runtime/expand_reference_runtime_probe_test.go.

// ── helpers ──────────────────────────────────────────────────────────────────

// innerFnGraph is a minimal exec-triggerable referenced function with its own
// __vars leaf "inner_val":
//
//	exec_in → setInner(variable.set "inner_val") → then
//	inInner(core.input __vars..inner_val) → getInner(get-field "field") → innerOut
func innerFnGraph(id string, version int) *ResolvedBlueprintGraph {
	jv := func(v string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"variable": json.RawMessage(`"` + v + `"`)}
	}
	jn := func(v string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"name": json.RawMessage(`"` + v + `"`)}
	}
	innerLeaf := varsLeafPrefix + ".inner_val"
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			inputNode("execEntry", "exec_in"),
			{ID: "setInner", Compute: coreVariableSet, Config: jv("inner_val"),
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "inInner", Compute: coreInput,
				Config:  jn(innerLeaf),
				Outputs: []BlueprintPort{dataOut("value")}},
			{ID: "getInner", Compute: "core.data.get-field@1",
				Config:  map[string]json.RawMessage{"path": json.RawMessage(`"field"`)},
				Inputs:  []BlueprintPort{dataIn("record")},
				Outputs: []BlueprintPort{dataOut("value")}},
			outputNode("innerOut", "inner_score"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "execEntry", FromPort: "then", ToNode: "setInner", ToPort: "exec_in"},
			{FromNode: "inInner", FromPort: "value", ToNode: "getInner", ToPort: "record"},
			{FromNode: "getInner", FromPort: "value", ToNode: "innerOut", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs: []BlueprintInterfacePin{
				{Name: "exec_in", Type: "exec", Kind: "exec", Required: true},
			},
			Outputs: []BlueprintInterfacePin{
				{Name: "then", Type: "exec", Kind: "exec"},
				{Name: "inner_score", Type: "any", Kind: "data"},
			},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

// outerFnGraph is an exec-triggerable referenced function with its own __vars
// leaf "outer_val" AND a nested reference to innerFnGraph ("bp-inner@1").
//
//	exec_in → setOuter(variable.set "outer_val") → callInner(bp-inner@1) → then
//	inOuter(core.input __vars..outer_val) → getOuter(get-field "x") → outerOut
func outerFnGraph(id string, version int) *ResolvedBlueprintGraph {
	jv := func(v string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"variable": json.RawMessage(`"` + v + `"`)}
	}
	jn := func(v string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"name": json.RawMessage(`"` + v + `"`)}
	}
	outerLeaf := varsLeafPrefix + ".outer_val"
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			inputNode("execEntry", "exec_in"),
			{ID: "setOuter", Compute: coreVariableSet, Config: jv("outer_val"),
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
			// Nested reference to bp-inner@1.
			refNode("callInner", "bp-inner", 1),
			{ID: "inOuter", Compute: coreInput,
				Config:  jn(outerLeaf),
				Outputs: []BlueprintPort{dataOut("value")}},
			{ID: "getOuter", Compute: "core.data.get-field@1",
				Config:  map[string]json.RawMessage{"path": json.RawMessage(`"x"`)},
				Inputs:  []BlueprintPort{dataIn("record")},
				Outputs: []BlueprintPort{dataOut("value")}},
			outputNode("outerOut", "outer_score"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "execEntry", FromPort: "then", ToNode: "setOuter", ToPort: "exec_in"},
			{FromNode: "setOuter", FromPort: "then", ToNode: "callInner", ToPort: "exec_in"},
			{FromNode: "inOuter", FromPort: "value", ToNode: "getOuter", ToPort: "record"},
			{FromNode: "getOuter", FromPort: "value", ToNode: "outerOut", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs: []BlueprintInterfacePin{
				{Name: "exec_in", Type: "exec", Kind: "exec", Required: true},
			},
			Outputs: []BlueprintInterfacePin{
				{Name: "then", Type: "exec", Kind: "exec"},
				{Name: "outer_score", Type: "any", Kind: "data"},
			},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

// nestedFetcher serves the three graphs needed for the nested-reference tests.
func nestedFetcher() *fakeFetcher {
	return &fakeFetcher{
		graphs: map[string]*ResolvedBlueprintGraph{
			"bp-inner@1": innerFnGraph("bp-inner", 1),
			"bp-outer@1": outerFnGraph("bp-outer", 1),
		},
	}
}

// callOuterFn builds a scene blueprint that calls bp-outer@1 once.
func callOuterFn(callID string) *BlueprintGraph {
	return &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
			refNode(callID, "bp-outer", 1),
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: callID, ToPort: "exec_in"},
		},
	}
}

// ── Test 1: Nested reference — no double-prefix, no collision ────────────────

// TestExpand_NestedReference_VarsLeaves_NoDoublePrefix checks that expanding
// two levels of reference (outer → inner), each with its own variable.set and
// core.input@1 __vars reader, produces exactly two DISTINCT promoted reader
// leaves and two DISTINCT variable.set leaves — one per level — without any
// double-prefix or collision between the outer and inner namespacing.
//
// This would break if:
//   - varNS is applied twice to the same leaf (double-prefix: "bpref1_..bpref2_..val")
//   - outer and inner share the same leaf name after expansion (collision)
func TestExpand_NestedReference_VarsLeaves_NoDoublePrefix(t *testing.T) {
	flat, diags := expandReferences(context.Background(), callOuterFn("scene_outer"), nestedFetcher())
	if len(diags) > 0 {
		t.Fatalf("expandReferences returned diagnostics: %+v", diags)
	}

	// Collect every promoted __vars reader (config.name starts with __vars..).
	type readerInfo struct{ id, leaf string }
	var readers []readerInfo
	for _, n := range flat.Nodes {
		if n.Compute != coreInput {
			continue
		}
		raw, ok := n.Config["name"]
		if !ok {
			continue
		}
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			continue
		}
		if strings.HasPrefix(name, varsLeafPrefix) {
			readers = append(readers, readerInfo{n.ID, name})
		}
	}

	// Expect exactly 2: one from outer, one from inner.
	if len(readers) != 2 {
		t.Fatalf("want 2 promoted __vars reader leaves (outer + inner), got %d: %+v", len(readers), readers)
	}

	leafSet := map[string]struct{}{}
	for _, r := range readers {
		leafSet[r.leaf] = struct{}{}
	}
	if len(leafSet) != 2 {
		t.Fatalf("want 2 DISTINCT reader leaves, got duplicates: %+v", readers)
	}

	// No leaf must contain the __vars prefix MORE THAN ONCE (double-prefix
	// would look like "__vars..__vars..<something>").
	for _, r := range readers {
		rest := r.leaf[len(varsLeafPrefix):]
		if strings.Contains(rest, varsLeafPrefix) {
			t.Fatalf("reader leaf %q contains a double __vars prefix — varNS applied twice", r.leaf)
		}
	}

	// Each variable.set must also be distinct — two levels, two leaves.
	setVars := map[string]struct{}{}
	for _, n := range flat.Nodes {
		if n.Compute == coreVariableSet {
			raw, ok := n.Config["variable"]
			if !ok {
				continue
			}
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				continue
			}
			setVars[v] = struct{}{}
		}
	}
	if len(setVars) != 2 {
		t.Fatalf("want 2 distinct variable.set leaves (outer + inner), got %d: %v", len(setVars), setVars)
	}

	// Each reader leaf must match its paired set: "__vars..<ns_var>" == "__vars.." + setVar.
	for _, n := range flat.Nodes {
		if n.Compute != coreInput {
			continue
		}
		raw, ok := n.Config["name"]
		if !ok {
			continue
		}
		var name string
		if err := json.Unmarshal(raw, &name); err != nil || !strings.HasPrefix(name, varsLeafPrefix) {
			continue
		}
		// The var name after the double-dot prefix.
		varName := name[len(varsLeaf("")):]
		if _, ok := setVars[varName]; !ok {
			t.Fatalf("promoted reader leaf %q has no matching variable.set (setVars=%v)", name, setVars)
		}
	}
}

// ── Test 2: variable.get inside a reference is namespaceable ─────────────────

// scoreForPlayerWithGet mirrors scoreForPlayerGraph but adds a core.variable.get@1
// on score_rows alongside the existing core.input@1 reader. Both must address
// the SAME namespaced leaf after expansion.
func scoreForPlayerWithGet(id string, version int) *ResolvedBlueprintGraph {
	jcfg := func(v string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"variable": json.RawMessage(`"` + v + `"`)}
	}
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			inputNode("inMatchId", "match_id"),
			inputNode("inPlayerId", "player_id"),
			{ID: "ein", Compute: coreInput,
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"exec_in"`)},
				Outputs: []BlueprintPort{execOut("then")}},
			{ID: "query", Compute: "core.db.query@1",
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("match_id"), dataIn("player_id")},
				Outputs: []BlueprintPort{execOut("then"), dataOut("rows")}},
			{ID: "setRows", Compute: coreVariableSet, Config: jcfg("score_rows"),
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
			// __vars.. reader via core.input (the bug epicentre).
			{ID: "inRows", Compute: coreInput,
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"` + scoreRowsLeaf + `"`)},
				Outputs: []BlueprintPort{dataOut("value")}},
			// __vars reader via core.variable.get (the angle Forge left open).
			{ID: "getRowsVar", Compute: coreVariableGet, Config: jcfg("score_rows"),
				Outputs: []BlueprintPort{dataOut("out")}},
			{ID: "getScore", Compute: "core.data.get-field@1",
				Config:  map[string]json.RawMessage{"path": json.RawMessage(`"0.score"`)},
				Inputs:  []BlueprintPort{dataIn("record")},
				Outputs: []BlueprintPort{dataOut("value")}},
			outputNode("scoreOut", "score"),
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

// TestExpand_VarsGet_NamespacedCoherentWithSet proves that a core.variable.get@1
// node inside a referenced function is namespaced coherently with its
// core.variable.set@1 pair: after expansion both must address the SAME
// instance-local leaf, not a global or unnamespaced one.
//
// This would break if namespaceVarsConfig missed the coreVariableGet case or
// applied a different ns function than the one used for coreVariableSet.
func TestExpand_VarsGet_NamespacedCoherentWithSet(t *testing.T) {
	fetcher := &fakeFetcher{
		graphs: map[string]*ResolvedBlueprintGraph{
			"bp-score-get@1": scoreForPlayerWithGet("bp-score-get", 1),
		},
	}
	scene := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
			refNode("sfp", "bp-score-get", 1),
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "sfp", ToPort: "exec_in"},
		},
	}

	flat, diags := expandReferences(context.Background(), scene, fetcher)
	if len(diags) > 0 {
		t.Fatalf("expandReferences returned diagnostics: %+v", diags)
	}

	// Find the set, the core.input reader, and the core.variable.get reader.
	var setVar, inputLeaf, getVar string

	for _, n := range flat.Nodes {
		switch n.Compute {
		case coreVariableSet:
			raw, ok := n.Config["variable"]
			if !ok {
				t.Fatal("variable.set missing config.variable")
			}
			if err := json.Unmarshal(raw, &setVar); err != nil {
				t.Fatalf("variable.set config.variable: %v", err)
			}
		case coreInput:
			raw, ok := n.Config["name"]
			if !ok {
				continue
			}
			var name string
			if err := json.Unmarshal(raw, &name); err != nil {
				continue
			}
			if strings.HasPrefix(name, varsLeafPrefix) {
				inputLeaf = name
			}
		case coreVariableGet:
			raw, ok := n.Config["variable"]
			if !ok {
				t.Fatal("variable.get missing config.variable")
			}
			if err := json.Unmarshal(raw, &getVar); err != nil {
				t.Fatalf("variable.get config.variable: %v", err)
			}
		}
	}

	if setVar == "" {
		t.Fatal("no variable.set node found after expansion")
	}
	if inputLeaf == "" {
		t.Fatal("no promoted core.input __vars reader found after expansion")
	}
	if getVar == "" {
		t.Fatal("no variable.get node found after expansion")
	}

	// set.variable and get.variable must be byte-identical (same namespaced leaf name).
	if setVar != getVar {
		t.Fatalf("set.variable %q != get.variable %q — variable.get not namespaced coherently with set", setVar, getVar)
	}

	// The promoted input reader leaf must equal "__vars.." + setVar.
	wantLeaf := varsLeaf(setVar)
	if inputLeaf != wantLeaf {
		t.Fatalf("promoted input reader leaf %q != expected %q (set var: %q)", inputLeaf, wantLeaf, setVar)
	}

	// None of the three must still carry the bare author name "score_rows".
	for _, name := range []string{setVar, getVar} {
		if name == "score_rows" {
			t.Fatalf("var %q was not namespaced — still the bare author name", name)
		}
	}
	if strings.HasSuffix(inputLeaf, ".score_rows") && !strings.Contains(inputLeaf[len(varsLeafPrefix):], "_") {
		// The leaf ends in .score_rows but has no instance prefix segment.
		t.Fatalf("input leaf %q looks un-namespaced (bare author suffix, no instance prefix)", inputLeaf)
	}
}

// ── Test 3: Author-name collision between two distinct blueprint functions ────

// rivalFnGraph is a function that ALSO uses "score_rows" as its internal var
// name — same as scoreForPlayerGraph — but is a totally distinct blueprint.
// A scene calling both in adjacent slots must not produce a single shared leaf.
func rivalFnGraph(id string, version int) *ResolvedBlueprintGraph {
	jcfg := func(v string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"variable": json.RawMessage(`"` + v + `"`)}
	}
	return &ResolvedBlueprintGraph{
		BlueprintID: id,
		Version:     version,
		Nodes: []BlueprintNode{
			inputNode("execEntry2", "exec_in"),
			{ID: "setRows2", Compute: coreVariableSet, Config: jcfg("score_rows"),
				Inputs:  []BlueprintPort{execIn("exec_in"), dataIn("value")},
				Outputs: []BlueprintPort{execOut("then")}},
			// Rival also has a core.input@1 reader on score_rows.
			{ID: "inRows2", Compute: coreInput,
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"` + scoreRowsLeaf + `"`)},
				Outputs: []BlueprintPort{dataOut("value")}},
			outputNode("rivalOut", "rival_score"),
		},
		Edges: []BlueprintEdge{
			{FromNode: "execEntry2", FromPort: "then", ToNode: "setRows2", ToPort: "exec_in"},
			{FromNode: "inRows2", FromPort: "value", ToNode: "rivalOut", ToPort: "value"},
		},
		Interface: BlueprintInterface{
			Inputs: []BlueprintInterfacePin{
				{Name: "exec_in", Type: "exec", Kind: "exec", Required: true},
			},
			Outputs: []BlueprintInterfacePin{
				{Name: "then", Type: "exec", Kind: "exec"},
				{Name: "rival_score", Type: "any", Kind: "data"},
			},
		},
		Purity: BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

// TestExpand_AuthorNameCollision_TwoDistinctFunctions proves that two distinct
// blueprint functions sharing the same internal variable name ("score_rows")
// are namespaced into separate leaves after expansion and do not clobber each
// other's state.
//
// This would break if varNS did not include the callID or the site counter,
// making both functions write/read the same leaf.
func TestExpand_AuthorNameCollision_TwoDistinctFunctions(t *testing.T) {
	fetcher := &fakeFetcher{
		graphs: map[string]*ResolvedBlueprintGraph{
			"bp-score@1": scoreForPlayerGraph("bp-score", 1),
			"bp-rival@1": rivalFnGraph("bp-rival", 1),
		},
	}
	scene := &BlueprintGraph{
		ID: "bp-scene",
		Nodes: []BlueprintNode{
			{ID: "start", Compute: coreEventOnStart, Outputs: []BlueprintPort{execOut("then")}},
			refNode("callScore", "bp-score", 1),
			refNode("callRival", "bp-rival", 1),
		},
		Edges: []BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "callScore", ToPort: "exec_in"},
			{FromNode: "start", FromPort: "then", ToNode: "callRival", ToPort: "exec_in"},
		},
	}

	flat, diags := expandReferences(context.Background(), scene, fetcher)
	if len(diags) > 0 {
		t.Fatalf("expandReferences returned diagnostics: %+v", diags)
	}

	// Collect all variable.set leaves.
	setVars := map[string]struct{}{}
	for _, n := range flat.Nodes {
		if n.Compute == coreVariableSet {
			raw, ok := n.Config["variable"]
			if !ok {
				continue
			}
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				continue
			}
			setVars[v] = struct{}{}
		}
	}
	// Two distinct blueprints each with one variable.set "score_rows" →
	// must produce 2 distinct namespaced leaf names, not 1 shared "score_rows".
	if len(setVars) != 2 {
		t.Fatalf("want 2 distinct variable.set leaves (one per distinct blueprint), got %d: %v", len(setVars), setVars)
	}
	for v := range setVars {
		if v == "score_rows" {
			t.Fatalf("bare unnamespaced 'score_rows' survives as a set leaf — two functions would clobber each other")
		}
	}

	// Collect all promoted __vars reader leaves.
	readerLeaves := map[string]struct{}{}
	for _, n := range flat.Nodes {
		if n.Compute != coreInput {
			continue
		}
		raw, ok := n.Config["name"]
		if !ok {
			continue
		}
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			continue
		}
		if strings.HasPrefix(name, varsLeafPrefix) {
			readerLeaves[name] = struct{}{}
		}
	}
	if len(readerLeaves) != 2 {
		t.Fatalf("want 2 distinct promoted reader leaves (one per distinct blueprint), got %d: %v", len(readerLeaves), readerLeaves)
	}

	// Each reader leaf must correspond to exactly one set leaf.
	for leaf := range readerLeaves {
		varName := leaf[len(varsLeaf("")):]
		if _, ok := setVars[varName]; !ok {
			t.Fatalf("reader leaf %q has no matching variable.set (sets=%v)", leaf, setVars)
		}
	}

	// The two set leaves and two reader leaves must be pairwise distinct
	// (4 different strings; already proven 2+2 above, cross-check collision).
	for sv := range setVars {
		for rv := range readerLeaves {
			// They are different types ("<name>" vs "__vars..<name>") so
			// they can never be equal — but check set names don't collide
			// across the two blueprints.
			_ = sv
			_ = rv
		}
	}
	allSetSlice := make([]string, 0, len(setVars))
	for v := range setVars {
		allSetSlice = append(allSetSlice, v)
	}
	if allSetSlice[0] == allSetSlice[len(allSetSlice)-1] {
		t.Fatalf("set leaves are not distinct: %v", allSetSlice)
	}
}

// ── Test 4 (compiler-level): degraded path — missing rows → null (not panic) ─

// TestExpand_VarsInput_EmptyRows_NullNotPanic is the guard that the degraded
// path (no rows written to the __vars leaf) produces a null walkPath result
// rather than panicking or producing a non-null value. We prove this at the
// compiler level: after expansion, getScore has a `record` input wired from
// the promoted reader, and the reader's leaf is namespaced. The runtime
// interpretation is proven end-to-end in the runtime probe test.
//
// This test is deliberately simple: it expands the reference and confirms the
// topology is consistent (the reader is promoted and wired). The null-value
// proof is in the runtime test.
func TestExpand_VarsInput_ExpandedTopology_ReaderWired(t *testing.T) {
	flat, diags := expandReferences(context.Background(), callScoreFn("sfp0"), scoreFetcher())
	if len(diags) > 0 {
		t.Fatalf("diagnostics: %+v", diags)
	}

	// Find the promoted reader.
	var readerID string
	for _, n := range flat.Nodes {
		if n.Compute != coreInput {
			continue
		}
		raw, ok := n.Config["name"]
		if !ok {
			continue
		}
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			continue
		}
		if strings.HasPrefix(name, varsLeafPrefix) {
			if readerID != "" {
				t.Fatal("more than one promoted __vars reader — unexpected in single-instance scene")
			}
			readerID = n.ID
		}
	}
	if readerID == "" {
		t.Fatal("promoted __vars reader absent — fix regression")
	}

	// Find getScore and verify its record input comes from the reader.
	var getScoreID string
	for _, n := range flat.Nodes {
		if n.Compute == "core.data.get-field@1" {
			getScoreID = n.ID
		}
	}
	if getScoreID == "" {
		t.Fatal("core.data.get-field@1 missing from expanded graph")
	}
	wired := false
	for _, e := range flat.Edges {
		if e.ToNode == getScoreID && e.ToPort == "record" && e.FromNode == readerID {
			wired = true
		}
	}
	if !wired {
		t.Fatalf("getScore (%s).record not wired from promoted reader (%s)", getScoreID, readerID)
	}
}
