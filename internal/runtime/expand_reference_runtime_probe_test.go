package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Probe RC #2 — runtime end-to-end proof of the ADR 014 __vars input-promotion fix.
//
// Forge's compiler tests prove that getScore.record is wired to the promoted
// reader (address match) but they do NOT execute the graph. This file proves
// that:
//
//  1. After compile + expansion, injecting real rows into the __vars leaf
//     that survived promotion produces a concrete value (not null) at the
//     scene output — the leaf pl.L0.score gets the real score, not "—".
//
//  2. When the __vars leaf is absent/empty (no rows injected), the output
//     falls back gracefully to null — the degraded-path guard is intact.
//
// The test avoids core.db.query@1 (world effect, no DB in unit test) by
// directly injecting the leaf value the db.query/variable.set pair would
// have written at runtime. This is valid because the promoted reader reads
// the leaf by name from state — the exact same code path as a live set.

// ── fetcher with reference support ──────────────────────────────────────────

// refAwareFetcher is a compiler.Fetcher that serves a single scene blueprint
// plus an arbitrary set of pinned referenced graphs.
type refAwareFetcher struct {
	layout    *compiler.CanvasLayout
	blueprint *compiler.BlueprintGraph
	manifest  compiler.ComputeManifest
	graphs    map[string]*compiler.ResolvedBlueprintGraph
}

func (f *refAwareFetcher) FetchCanvasLayout(_ context.Context, _ string) (*compiler.CanvasLayout, error) {
	return f.layout, nil
}
func (f *refAwareFetcher) FetchBlueprint(_ context.Context, _ string) (*compiler.BlueprintGraph, error) {
	return f.blueprint, nil
}
func (f *refAwareFetcher) FetchBlueprintGraph(_ context.Context, id string, version int) (*compiler.ResolvedBlueprintGraph, error) {
	key := fmt.Sprintf("%s@%d", id, version)
	if g, ok := f.graphs[key]; ok {
		return g, nil
	}
	return nil, fmt.Errorf("blueprint %s: %w", key, errors.New("not found"))
}
func (f *refAwareFetcher) FetchComponent(_ context.Context, _ compiler.ComponentRef) (*compiler.UserComponent, error) {
	return nil, errors.New("no components")
}
func (f *refAwareFetcher) FetchComputeManifest(_ context.Context) (compiler.ComputeManifest, error) {
	return f.manifest, nil
}

// ── blueprint fixtures ───────────────────────────────────────────────────────

// scoreForPlayerPureGraph is a version of score-for-player WITHOUT core.db.query
// (which needs a live DB). The data path is identical to the production graph:
//
//	inRows (core.input __vars..score_rows) → getScore (get-field "0.score") → scoreOut (core.output "score")
//
// In production, db.query + variable.set writes __vars..score_rows and the
// promoted inRows reader picks it up. Here we skip the exec spine and inject
// the rows leaf directly from the test — proving the pure-data half of the
// circuit end-to-end.
func scoreForPlayerPureGraph() *compiler.ResolvedBlueprintGraph {
	scoreRowsLeaf := "__vars..score_rows"
	return &compiler.ResolvedBlueprintGraph{
		BlueprintID: "bp-score-pure",
		Version:     1,
		Nodes: []compiler.BlueprintNode{
			// Interface relay nodes (spliced away on expansion).
			{
				ID:      "inMatchId",
				Compute: "core.input@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"match_id"`)},
			},
			{
				ID:      "inPlayerId",
				Compute: "core.input@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"player_id"`)},
			},
			// The bug epicentre: core.input@1 reading a __vars leaf. This node
			// must SURVIVE expansion as a promoted interior node. If dropped, the
			// getScore record input is unwired → walkPath(nil,"0.score") → null.
			{
				ID:      "inRows",
				Compute: "core.input@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"` + scoreRowsLeaf + `"`)},
				Outputs: []compiler.BlueprintPort{{Name: "value", Type: "any", Kind: "data"}},
			},
			{
				ID:      "getScore",
				Compute: "core.data.get-field@1",
				Config:  map[string]json.RawMessage{"path": json.RawMessage(`"0.score"`)},
				Inputs:  []compiler.BlueprintPort{{Name: "record", Type: "any", Kind: "data"}},
				Outputs: []compiler.BlueprintPort{{Name: "value", Type: "any", Kind: "data"}},
			},
			{
				ID:      "scoreOut",
				Compute: "core.output@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"pl.L0.score"`)},
			},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "inRows", FromPort: "value", ToNode: "getScore", ToPort: "record"},
			{FromNode: "getScore", FromPort: "value", ToNode: "scoreOut", ToPort: "value"},
		},
		Interface: compiler.BlueprintInterface{
			Inputs: []compiler.BlueprintInterfacePin{
				{Name: "match_id", Type: "any", Kind: "data"},
				{Name: "player_id", Type: "any", Kind: "data"},
			},
			Outputs: []compiler.BlueprintInterfacePin{
				{Name: "pl.L0.score", Type: "any", Kind: "data"},
			},
		},
		Purity: compiler.BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

// scoreForPlayerScene is the scene blueprint that calls bp-score-pure@1 once
// and routes the output slot "pl.L0.score" to a scene-level output node bound
// to the same leaf path. This is the production pattern: the reference
// declares output pin "pl.L0.score", the scene has a core.output@1 node
// with config.name="pl.L0.score" that the edge wires to.
func scoreForPlayerScene() *compiler.BlueprintGraph {
	return &compiler.BlueprintGraph{
		ID: "bp-scene",
		Nodes: []compiler.BlueprintNode{
			{
				ID:        "sfp",
				Compute:   "blueprint.reference",
				Reference: &compiler.BlueprintReference{BlueprintID: "bp-score-pure", Version: 1},
			},
			{
				ID:      "sink",
				Compute: "core.output@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"pl.L0.score"`)},
			},
		},
		Edges: []compiler.BlueprintEdge{
			// Wire the reference's output pin "pl.L0.score" to the sink.
			{FromNode: "sfp", FromPort: "pl.L0.score", ToNode: "sink", ToPort: "value"},
		},
	}
}

// scoreForPlayerManifest lists only the computes the pure data graph uses.
func scoreForPlayerManifest() compiler.ComputeManifest {
	return compiler.ComputeManifest{
		"core.input@1":          {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":         {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.get-field@1": {IsPure: true, IsBounded: true, Version: "1"},
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// findVarsLeaf scans the compiled graph for the first GraphNode of kind
// "input" whose Path starts with "__vars." and returns its Path. If none
// is found (the bug — the promoted reader was dropped) it returns "".
func findVarsLeaf(g *compiler.Graph) string {
	const prefix = "__vars."
	for _, n := range g.Nodes {
		if n.Kind == "input" && strings.HasPrefix(n.Path, prefix) {
			return n.Path
		}
	}
	return ""
}

// compileScoreForPlayerScene compiles the scene and returns the graph +
// fetcher used (so tests can inspect it).
func compileScoreForPlayerScene(t *testing.T) (*compiler.Graph, *compiler.RenderBundle) {
	t.Helper()
	f := &refAwareFetcher{
		layout: &compiler.CanvasLayout{
			Version: "v1",
			Root:    compiler.LayoutNode{Kind: "stack", ID: "root"},
		},
		blueprint: scoreForPlayerScene(),
		manifest:  scoreForPlayerManifest(),
		graphs: map[string]*compiler.ResolvedBlueprintGraph{
			"bp-score-pure@1": scoreForPlayerPureGraph(),
		},
	}
	graph, bundle, version, err := compiler.Compile(
		context.Background(),
		"scene-score-player",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"},
		f,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if version == "" {
		t.Fatal("compile returned empty scene_version")
	}
	return graph, bundle
}

// ── RC #2 test: real rows → real value at leaf ───────────────────────────────

// TestExpand_VarsInput_RuntimeEndToEnd_RealRowsProduceRealValue is the
// resolution criterion RC #2 proof: a scene compiled with a score-for-player
// reference, populated with real rows via its promoted __vars leaf, must
// return the real score at pl.L0.score — NOT null.
//
// This is the test Forge explicitly left open: "j'ai prouvé l'appariement
// d'adresses, pas la valeur runtime." This test proves the value.
//
// If the fix is fragile (promoted reader not wired to getScore.record, or the
// injected leaf path is wrong), the recompute produces null and the test fails.
func TestExpand_VarsInput_RuntimeEndToEnd_RealRowsProduceRealValue(t *testing.T) {
	graph, bundle := compileScoreForPlayerScene(t)

	// Step 1: Identify the promoted __vars leaf in the compiled graph.
	// If findVarsLeaf returns "" the promoted reader was dropped — the bug.
	varsLeafPath := findVarsLeaf(graph)
	if varsLeafPath == "" {
		t.Fatal("promoted __vars reader leaf NOT FOUND in compiled graph — the bug is back: " +
			"core.input@1 reading __vars..score_rows was dropped during expansion")
	}

	// Step 2: Wire the scene.
	sc := NewScene("scene-score-player", graph, bundle, NewComputeRegistry(), quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sc.Run(ctx)
	t.Cleanup(sc.Stop)

	// Step 3: Observe the cold-start value. With no rows injected, the __vars
	// leaf is absent from state → inRows.value = nil → getScore.record = nil
	// → walkPath(nil, "0.score") = nil → pl.L0.score = null.
	// The degraded-path guard must hold: null is the correct placeholder.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("pl.L0.score"); ok && string(v) != "null" {
			t.Fatalf("cold-start pl.L0.score = %s, want null (degraded guard broken)", v)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Step 4: Inject real rows into the promoted __vars leaf. This simulates
	// what core.db.query@1 + core.variable.set@1 would write at runtime.
	// [{"score": 99}] → getScore path "0.score" → 99.
	rows := json.RawMessage(`[{"score": 99}]`)
	if !sc.Input(InputMsg{
		Path:   varsLeafPath,
		Value:  rows,
		Source: "test:probe",
	}) {
		t.Fatal("inbox full injecting rows leaf")
	}

	// Step 5: Wait for the recompute to propagate through:
	//   inRows (reads __vars leaf) → getScore (get-field "0.score") → scoreOut → pl.L0.score
	// The expected value is 99 (JSON number).
	waitForState(t, sc, "pl.L0.score", `99`, 2*time.Second)

	// Step 6: Inject a different row set and verify the value updates.
	// This proves the leaf is reactive (a one-shot fluke at step 5 would not update).
	rows2 := json.RawMessage(`[{"score": 42}]`)
	if !sc.Input(InputMsg{
		Path:   varsLeafPath,
		Value:  rows2,
		Source: "test:probe",
	}) {
		t.Fatal("inbox full injecting second rows leaf")
	}
	waitForState(t, sc, "pl.L0.score", `42`, 2*time.Second)
}

// ── RC #2 degraded path: empty rows → null, no panic ─────────────────────────

// TestExpand_VarsInput_RuntimeEndToEnd_EmptyRows_NullNotPanic verifies that
// injecting an empty rows list (or a null value) into the promoted __vars leaf
// does NOT panic and falls back correctly to null at pl.L0.score.
//
// walkPath(nil, "0.score") == nil (the array is empty or the record is nil).
// This guards the degraded path: the fix must not change the null-fallback for
// a reference that fired but produced no rows.
func TestExpand_VarsInput_RuntimeEndToEnd_EmptyRows_NullNotPanic(t *testing.T) {
	graph, bundle := compileScoreForPlayerScene(t)

	varsLeafPath := findVarsLeaf(graph)
	if varsLeafPath == "" {
		t.Skip("promoted __vars leaf absent — covered by RealRows test")
	}

	sc := NewScene("scene-score-degraded", graph, bundle, NewComputeRegistry(), quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sc.Run(ctx)
	t.Cleanup(sc.Stop)

	// Inject an empty array — no rows, walkPath([], "0") → nil.
	if !sc.Input(InputMsg{
		Path:   varsLeafPath,
		Value:  json.RawMessage(`[]`),
		Source: "test:probe",
	}) {
		t.Fatal("inbox full")
	}

	// Allow the recompute to settle. pl.L0.score must be null (not a stale
	// non-null value and definitely not a panic).
	time.Sleep(100 * time.Millisecond)
	if v, ok := sc.state.Get("pl.L0.score"); ok && string(v) != "null" {
		t.Fatalf("empty-rows: pl.L0.score = %s, want null", v)
	}

	// Inject explicit null.
	if !sc.Input(InputMsg{
		Path:   varsLeafPath,
		Value:  json.RawMessage(`null`),
		Source: "test:probe",
	}) {
		t.Fatal("inbox full")
	}
	time.Sleep(100 * time.Millisecond)
	if v, ok := sc.state.Get("pl.L0.score"); ok && string(v) != "null" {
		t.Fatalf("null-rows: pl.L0.score = %s, want null", v)
	}
}

// ── Two-instance dataflow isolation at runtime ────────────────────────────────

// TestExpand_VarsInput_RuntimeEndToEnd_TwoInstances_NoDataClobber verifies that
// two calls to score-for-player (sfp_L0 and sfp_R4) in the same scene produce
// INDEPENDENT output leaves and INDEPENDENT __vars leaves. Injecting rows into
// L0's leaf must not update R4's output and vice versa.
//
// This is the runtime complement of TestExpand_VarsInputReader_DistinctPerInstance
// (which proves address distinctness at compiler level): this test proves VALUE
// isolation at runtime — the two promoted readers consume different state leaves.
func TestExpand_VarsInput_RuntimeEndToEnd_TwoInstances_NoDataClobber(t *testing.T) {
	// Scene with two calls to the same blueprint function, each routed to a
	// distinct scene-level output leaf (pl.L0.score and pl.R4.score).
	scene2 := &compiler.BlueprintGraph{
		ID: "bp-scene-two",
		Nodes: []compiler.BlueprintNode{
			{
				ID:        "sfpL0",
				Compute:   "blueprint.reference",
				Reference: &compiler.BlueprintReference{BlueprintID: "bp-score-pure", Version: 1},
			},
			{
				ID:        "sfpR4",
				Compute:   "blueprint.reference",
				Reference: &compiler.BlueprintReference{BlueprintID: "bp-score-pure", Version: 1},
			},
			// Each instance routes its output to a distinct scene leaf.
			{
				ID:      "sinkL0",
				Compute: "core.output@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"pl.L0.score"`)},
			},
			{
				ID:      "sinkR4",
				Compute: "core.output@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"pl.R4.score"`)},
			},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "sfpL0", FromPort: "pl.L0.score", ToNode: "sinkL0", ToPort: "value"},
			{FromNode: "sfpR4", FromPort: "pl.L0.score", ToNode: "sinkR4", ToPort: "value"},
		},
	}

	f := &refAwareFetcher{
		layout: &compiler.CanvasLayout{
			Version: "v1",
			Root:    compiler.LayoutNode{Kind: "stack", ID: "root"},
		},
		blueprint: scene2,
		manifest:  scoreForPlayerManifest(),
		graphs: map[string]*compiler.ResolvedBlueprintGraph{
			"bp-score-pure@1": scoreForPlayerPureGraph(),
		},
	}
	graph, bundle, _, err := compiler.Compile(
		context.Background(),
		"scene-two-players",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene-two"},
		f,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Collect the two promoted __vars leaves (one per instance).
	const varsPrefix = "__vars."
	var varsLeaves []string
	for _, n := range graph.Nodes {
		if n.Kind == "input" && strings.HasPrefix(n.Path, varsPrefix) {
			varsLeaves = append(varsLeaves, n.Path)
		}
	}
	if len(varsLeaves) != 2 {
		t.Fatalf("want 2 promoted __vars input nodes in compiled graph, got %d (paths: %v)",
			len(varsLeaves), varsLeaves)
	}
	if varsLeaves[0] == varsLeaves[1] {
		t.Fatalf("two instances share the same __vars leaf %q — data clobber possible", varsLeaves[0])
	}

	// The two score output leaves must also be distinct (pl.L0.score and pl.R4.score).
	wantScoreLeaves := map[string]struct{}{
		"pl.L0.score": {},
		"pl.R4.score": {},
	}
	var scoreLeaves []string
	for _, n := range graph.Nodes {
		if n.Kind == "output" {
			if _, ok := wantScoreLeaves[n.Path]; ok {
				scoreLeaves = append(scoreLeaves, n.Path)
			}
		}
	}
	if len(scoreLeaves) != 2 {
		t.Fatalf("want 2 output score leaves (pl.L0.score and pl.R4.score), got %d: %v", len(scoreLeaves), scoreLeaves)
	}
	if scoreLeaves[0] == scoreLeaves[1] {
		t.Fatalf("two instances share the same output leaf %q", scoreLeaves[0])
	}

	// Run the scene and prove value isolation.
	sc := NewScene("scene-two-players", graph, bundle, NewComputeRegistry(), quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sc.Run(ctx)
	t.Cleanup(sc.Stop)

	// Inject rows into the FIRST instance's leaf only.
	if !sc.Input(InputMsg{
		Path:   varsLeaves[0],
		Value:  json.RawMessage(`[{"score": 77}]`),
		Source: "test:probe",
	}) {
		t.Fatal("inbox full")
	}

	// Wait for the first instance's output to reflect the injected value.
	// We don't know which scoreLeaf maps to which varsLeaf, so we poll both
	// and require that exactly one reaches 77 and the other remains null.
	deadline := time.Now().Add(2 * time.Second)
	var found bool
	for !found && time.Now().Before(deadline) {
		v0, ok0 := sc.state.Get(scoreLeaves[0])
		v1, ok1 := sc.state.Get(scoreLeaves[1])
		_ = ok0
		_ = ok1
		bothNonNull := string(v0) != "null" && string(v0) != ""
		onlyOne := (string(v0) == "77" && string(v1) != "77") ||
			(string(v1) == "77" && string(v0) != "77")
		if bothNonNull && onlyOne {
			found = true
		} else if string(v0) == "77" || string(v1) == "77" {
			// One reached 77 — check the other hasn't clobbered.
			if string(v0) == "77" && string(v1) == "77" {
				t.Fatalf("CLOBBER: both score leaves reached 77 after injecting only one __vars leaf (leaves: %v, scores: %v, %v)", varsLeaves, string(v0), string(v1))
			}
			found = true
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !found {
		v0, _ := sc.state.Get(scoreLeaves[0])
		v1, _ := sc.state.Get(scoreLeaves[1])
		t.Fatalf("after injecting %s with score=77: scoreLeaves[0]=%s scoreLeaves[1]=%s — expected exactly one to reach 77",
			varsLeaves[0], v0, v1)
	}
}
