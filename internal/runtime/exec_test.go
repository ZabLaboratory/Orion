package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// Tests for the exec interpreter (ADR 003 §3.1, issue #82): explicit
// continuations on the scene goroutine, time-slicing as fairness
// (never kill), B5 back-pressure on NEW fires only, B9 `__vars`
// isolation, and the park/resume seam issue #83 builds on.

// fakeExecMetrics is a race-safe ExecMetrics sink for assertions.
type fakeExecMetrics struct {
	mu            sync.Mutex
	shed          int
	preempt       int
	parked        int
	wheel         int
	parkDropped   map[string]int // by reason ("duplicate_key", "cap")
	resumeStale   int
	resumeUnknown int
	// cpuBySceneVersion accumulates ExecCPUSeconds per scene_version
	// label (B7, issue #89); cpuCalls counts slice reports.
	cpuBySceneVersion map[string]float64
	cpuCalls          int
}

func (f *fakeExecMetrics) ExecCPUSeconds(_, sceneVersion string, seconds float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cpuBySceneVersion == nil {
		f.cpuBySceneVersion = map[string]float64{}
	}
	f.cpuBySceneVersion[sceneVersion] += seconds
	f.cpuCalls++
}

func (f *fakeExecMetrics) cpu() (byVersion map[string]float64, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	byVersion = make(map[string]float64, len(f.cpuBySceneVersion))
	for k, v := range f.cpuBySceneVersion {
		byVersion[k] = v
	}
	return byVersion, f.cpuCalls
}

func (f *fakeExecMetrics) ExecResumeUnknown(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeUnknown++
}

func (f *fakeExecMetrics) unknown() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resumeUnknown
}

func (f *fakeExecMetrics) ExecTimerWheelSize(_ string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wheel = n
}

func (f *fakeExecMetrics) ExecParkDropped(_, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parkDropped == nil {
		f.parkDropped = map[string]int{}
	}
	f.parkDropped[reason]++
}

func (f *fakeExecMetrics) ExecResumeStale(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeStale++
}

func (f *fakeExecMetrics) wheelSize() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wheel
}

func (f *fakeExecMetrics) droppedBy(reason string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.parkDropped[reason]
}

func (f *fakeExecMetrics) stale() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resumeStale
}

func (f *fakeExecMetrics) ExecEventShed(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shed++
}

func (f *fakeExecMetrics) ExecTaskPreempt(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preempt++
}

func (f *fakeExecMetrics) ExecParkedTasks(_ string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parked = n
}

func (f *fakeExecMetrics) counts() (shed, preempt, parked int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shed, f.preempt, f.parked
}

// waitForState polls the scene state (State is internally locked, so
// the cross-goroutine read is race-safe) until the leaf equals want.
func waitForState(t *testing.T, sc *Scene, path, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get(path); ok && string(v) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get(path)
	t.Fatalf("leaf %s = %s, want %s", path, v, want)
}

func startScene(t *testing.T, sc *Scene) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sc.Run(ctx)
	t.Cleanup(sc.Stop)
}

func mustFire(t *testing.T, sc *Scene, entry string) {
	t.Helper()
	if !sc.FireExec(entry, "test") {
		t.Fatalf("inbox full firing %s", entry)
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// varsGraph builds a graph exposing `__vars.bp.<name>` leaves as
// input-kind nodes (the shape the compiler partition will emit for
// `variable.get`), plus pure helpers used by the exec tests.
func varsGraph(id string) *compiler.Graph {
	return &compiler.Graph{
		SceneID:      id,
		SceneVersion: "sha256:exec-test",
		Nodes: []compiler.GraphNode{
			{ID: "in.cond", Kind: "input", Path: "flags.cond"},
			{ID: "in.run", Kind: "input", Path: "flags.run"},
			{ID: "in.score", Kind: "input", Path: "score.team_a"},
			{ID: "var.counter", Kind: "input", Path: "__vars.bp.counter"},
			{ID: "var.done", Kind: "input", Path: "__vars.bp.done"},
			{ID: "lit.one", Kind: "input", Path: "lit.one"},
			{ID: "lit.five", Kind: "input", Path: "lit.five"},
			{ID: "add.counter", Kind: "computed", Compute: "core.math.add@1",
				Upstream: []string{"var.counter", "lit.one"},
				Inputs: []compiler.GraphInput{
					{From: "var.counter", Port: "a"}, {From: "lit.one", Port: "b"},
				}},
			{ID: "add.done", Kind: "computed", Compute: "core.math.add@1",
				Upstream: []string{"var.done", "lit.one"},
				Inputs: []compiler.GraphInput{
					{From: "var.done", Port: "a"}, {From: "lit.one", Port: "b"},
				}},
			{ID: "lt.counter", Kind: "computed", Compute: "core.compare.less-than@1",
				Upstream: []string{"var.counter", "lit.five"},
				Inputs: []compiler.GraphInput{
					{From: "var.counter", Port: "a"}, {From: "lit.five", Port: "b"},
				}},
		},
		Defaults: map[string]json.RawMessage{
			"flags.cond":        raw(`false`),
			"flags.run":         raw(`true`),
			"score.team_a":      raw(`0`),
			"__vars.bp.counter": raw(`0`),
			"__vars.bp.done":    raw(`0`),
			"lit.one":           raw(`1`),
			"lit.five":          raw(`5`),
		},
	}
}

func execScene(t *testing.T, id string, prog *ExecProgram) *Scene {
	t.Helper()
	sc := NewScene(id, varsGraph(id), &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(prog)
	return sc
}

// TestExec_VariableGetReadsCrossTick is the compiler→runtime end-to-end the
// hand-built varsGraph could not give: it drives the REAL compiler on a
// blueprint chaining variable.get + math.add + variable.set off on-tick, then
// runs the compiled artefact through a live Scene over several ticks.
//
// It is the regression guard for the on-air freeze bug: variable.get fell into
// nodeLeafPath's default (Path="") → classified input → demandValue read the
// node-id leaf (never written) → 0 every tick → add(0,1)=1 forever. With the
// leaf bound to `__vars..counter` (byte-identical to execVariableSet's empty-
// key write) the read sees the prior tick's write and the counter climbs.
func TestExec_VariableGetReadsCrossTick(t *testing.T) {
	ep := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
	}
	dp := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "any", Kind: "data"}
	}
	bp := &compiler.BlueprintGraph{
		ID: "bp-xtick",
		Nodes: []compiler.BlueprintNode{
			{ID: "tick", Compute: "core.event.on-tick@1",
				Outputs: []compiler.BlueprintPort{ep("then"), dp("delta_seconds")}},
			{ID: "get", Compute: "core.variable.get@1",
				Config:  map[string]json.RawMessage{"name": raw(`"counter"`)},
				Outputs: []compiler.BlueprintPort{dp("out")}},
			{ID: "one", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": raw(`1`)},
				Outputs: []compiler.BlueprintPort{dp("out")}},
			{ID: "add", Compute: "core.math.add@1",
				Inputs:  []compiler.BlueprintPort{dp("a"), dp("b")},
				Outputs: []compiler.BlueprintPort{dp("out")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": raw(`"counter"`)},
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("value")},
				Outputs: []compiler.BlueprintPort{ep("then")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "tick", FromPort: "then", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "get", FromPort: "out", ToNode: "add", ToPort: "a"},
			{FromNode: "one", FromPort: "out", ToNode: "add", ToPort: "b"},
			{FromNode: "add", FromPort: "out", ToNode: "set", ToPort: "value"},
		},
	}
	fetcher := &stubFetcher{
		layout:    &compiler.CanvasLayout{Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		blueprint: bp,
		manifest: compiler.ComputeManifest{
			"core.event.on-tick@1": {IsPure: true, IsBounded: true, Version: "1"},
			"core.variable.get@1":  {IsPure: true, IsBounded: true, Version: "1"},
			"core.variable.set@1":  {IsPure: true, IsBounded: true, Version: "1"},
			"core.literal@1":       {IsPure: true, IsBounded: true, Version: "1"},
			"core.math.add@1":      {IsPure: true, IsBounded: true, Version: "1"},
		},
	}

	graph, bundle, _, err := compiler.Compile(context.Background(), "xtick-scene",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-xtick"}, fetcher)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// The compiler must bind get's leaf byte-identically to set's write.
	var getNode *compiler.GraphNode
	for i := range graph.Nodes {
		if graph.Nodes[i].Compute == "core.variable.get@1" {
			getNode = &graph.Nodes[i]
		}
	}
	if getNode == nil {
		t.Fatal("compiled graph has no variable.get data node")
	}
	if getNode.Path != "__vars..counter" {
		t.Fatalf("variable.get leaf = %q, want __vars..counter (single-blueprint empty key)", getNode.Path)
	}

	progs, err := ExecProgramsFromGraph(graph)
	if err != nil || len(progs) == 0 {
		t.Fatalf("exec programs: %v (n=%d)", err, len(progs))
	}

	sc := NewScene("xtick-scene", graph, bundle, NewComputeRegistry(), quietLogger())
	for _, p := range progs {
		sc.InstallExec(p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sc.Run(ctx)
	t.Cleanup(sc.Stop)

	tickEntry := ""
	for _, p := range progs {
		for name, e := range p.Entrypoints {
			if e.Kind == EntryOnTick {
				tickEntry = name
			}
		}
	}
	if tickEntry == "" {
		t.Fatal("no on-tick entrypoint compiled")
	}

	leaf := "__vars..counter"
	for i := 1; i <= 3; i++ {
		mustFire(t, sc, tickEntry)
		waitForState(t, sc, leaf, fmt.Sprintf("%d", i), time.Second)
	}
}

func varSet(id, name string, data []ExecDataInput, next map[string]ExecTarget) *ExecNode {
	return &ExecNode{
		ID: id, Op: OpVariableSet,
		Config: map[string]json.RawMessage{"name": raw(`"` + name + `"`)},
		Data:   data, Next: next,
	}
}

// TestExec_BranchBothArms: `branch` pulls its condition through the
// data layer and continues down exactly one arm (criterion 3).
func TestExec_BranchBothArms(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"br": {ID: "br", Op: OpBranch,
				Data: []ExecDataInput{{Port: "condition", From: "in.cond"}},
				Next: map[string]ExecTarget{
					"true":  {Node: "set.t"},
					"false": {Node: "set.f"},
				}},
			"set.t": varSet("set.t", "arm", nil, nil),
			"set.f": varSet("set.f", "arm", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "br"}}},
	}
	prog.Nodes["set.t"].Config["value"] = raw(`"true-arm"`)
	prog.Nodes["set.f"].Config["value"] = raw(`"false-arm"`)

	sc := execScene(t, "branch-test", prog)
	startScene(t, sc)

	mustFire(t, sc, "e") // flags.cond defaults to false
	waitForState(t, sc, "__vars.bp.arm", `"false-arm"`, time.Second)

	sc.Input(InputMsg{Path: "flags.cond", Value: raw(`true`), Source: "test"})
	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.arm", `"true-arm"`, time.Second)
}

// TestExec_SequenceOrder: `sequence` fires then_0..then_2 in declared
// order — proven by the append-ordered `__debug` print ring, never by
// map iteration (criterion 3).
func TestExec_SequenceOrder(t *testing.T) {
	printNode := func(id, msg string) *ExecNode {
		return &ExecNode{ID: id, Op: OpPrint,
			Config: map[string]json.RawMessage{"message": raw(`"` + msg + `"`)}}
	}
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"seq": {ID: "seq", Op: OpSequence, Next: map[string]ExecTarget{
				"then_0": {Node: "p0"},
				"then_1": {Node: "p1"},
				"then_2": {Node: "p2"},
			}},
			"p0": printNode("p0", "first"),
			"p1": printNode("p1", "second"),
			"p2": printNode("p2", "third"),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "seq"}}},
	}
	sc := execScene(t, "seq-test", prog)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__debug.bp.print", `["first","second","third"]`, time.Second)
}

// TestExec_ForLoop_ContinuationResumesAcrossSlices: the continuation
// proof. A 0..9 loop runs under a 3-step slice, so the task is
// preempted and re-enqueued many times — and still completes with the
// exact final state, with `orion_task_preempt_total` incremented and
// nothing killed (criteria 3 + 6; the maintainer's loop test shape,
// criterion 4, minus the push API which is a later phase).
func TestExec_ForLoop_ContinuationResumesAcrossSlices(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"loop": {ID: "loop", Op: OpForLoop,
				Config: map[string]json.RawMessage{"first": raw(`0`), "last": raw(`9`)},
				Next: map[string]ExecTarget{
					"body":      {Node: "set"},
					"completed": {Node: "set.done"},
				}},
			"set": varSet("set", "counter",
				[]ExecDataInput{{Port: "value", From: "loop", FromPort: "index"}}, nil),
			"set.done": varSet("set.done", "loop_done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "loop"}}},
	}
	prog.Nodes["set.done"].Config["value"] = raw(`true`)

	metrics := &fakeExecMetrics{}
	sc := execScene(t, "loop-test", prog)
	sc.SetExecMetrics(metrics)
	sc.SetExecSlicing(3, time.Hour) // force step-budget preemption only
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.loop_done", `true`, time.Second)
	waitForState(t, sc, "__vars.bp.counter", `9`, time.Second)

	_, preempt, _ := metrics.counts()
	if preempt == 0 {
		t.Fatal("expected at least one time-slice preemption (3-step slice over a 10-iteration loop)")
	}
}

// TestExec_WhileTerminatesOnCondition: `while` re-pulls its condition
// each iteration through demand evaluation, observing the body's
// `variable.set` effects (criterion 3). counter 0→5, condition
// less-than(counter, 5) goes false, `completed` fires.
func TestExec_WhileTerminatesOnCondition(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"while": {ID: "while", Op: OpWhile,
				Data: []ExecDataInput{{Port: "condition", From: "lt.counter"}},
				Next: map[string]ExecTarget{
					"body":      {Node: "inc"},
					"completed": {Node: "set.done"},
				}},
			"inc": varSet("inc", "counter",
				[]ExecDataInput{{Port: "value", From: "add.counter"}}, nil),
			"set.done": varSet("set.done", "while_done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "while"}}},
	}
	prog.Nodes["set.done"].Config["value"] = raw(`true`)

	sc := execScene(t, "while-test", prog)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.while_done", `true`, time.Second)
	waitForState(t, sc, "__vars.bp.counter", `5`, time.Second)
}

// TestExec_ForEach: per-iteration `element` + `index` pins bound in
// the task environment (criterion 3).
func TestExec_ForEach(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"each": {ID: "each", Op: OpForEach,
				Config: map[string]json.RawMessage{"list": raw(`["x","y","z"]`)},
				Next: map[string]ExecTarget{
					"body":      {Node: "set.el"},
					"completed": {Node: "set.done"},
				}},
			"set.el": varSet("set.el", "last_element",
				[]ExecDataInput{{Port: "value", From: "each", FromPort: "element"}},
				map[string]ExecTarget{"then": {Node: "set.idx"}}),
			"set.idx": varSet("set.idx", "last_index",
				[]ExecDataInput{{Port: "value", From: "each", FromPort: "index"}}, nil),
			"set.done": varSet("set.done", "each_done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "each"}}},
	}
	prog.Nodes["set.done"].Config["value"] = raw(`true`)

	sc := execScene(t, "foreach-test", prog)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.each_done", `true`, time.Second)
	waitForState(t, sc, "__vars.bp.last_element", `"z"`, time.Second)
	waitForState(t, sc, "__vars.bp.last_index", `2`, time.Second)
}

// TestExec_GateStartClosedThenOpened: gate state is persistent per
// node (`__nodestate.<id>`), `enter` passes iff open, `open` mutates
// without firing (criterion 3).
func TestExec_GateStartClosedThenOpened(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"gate": {ID: "gate", Op: OpGate,
				Config: map[string]json.RawMessage{"start_closed": raw(`true`)},
				Next:   map[string]ExecTarget{"exit": {Node: "set"}}},
			"set": varSet("set", "passed", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"enter": {Target: ExecTarget{Node: "gate", Port: "enter"}},
			"open":  {Target: ExecTarget{Node: "gate", Port: "open"}},
		},
	}
	prog.Nodes["set"].Config["value"] = raw(`true`)

	sc := execScene(t, "gate-test", prog)
	startScene(t, sc)

	mustFire(t, sc, "enter") // closed — must not pass
	mustFire(t, sc, "open")
	waitForState(t, sc, "__nodestate.gate", `{"open":true}`, time.Second)
	if v, ok := sc.state.Get("__vars.bp.passed"); ok {
		t.Fatalf("gate passed while closed: %s", v)
	}
	mustFire(t, sc, "enter")
	waitForState(t, sc, "__vars.bp.passed", `true`, time.Second)
}

// TestExec_ParkSeam_SequenceContinuesWhileChildParked: the latent
// seam issue #83 (delay / timer wheel) and phase 3 (async effects)
// build on. A synthetic latent op parks its chain: the sequence
// continues with the next sibling (UE latent semantics, ADR 003
// §3.1.3), `orion_parked_tasks` reports 1, and a resume delivered as
// an inbox message resumes EXACTLY the parked continuation.
func TestExec_ParkSeam_SequenceContinuesWhileChildParked(t *testing.T) {
	printNode := func(id, msg string) *ExecNode {
		return &ExecNode{ID: id, Op: OpPrint,
			Config: map[string]json.RawMessage{"message": raw(`"` + msg + `"`)}}
	}
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"seq": {ID: "seq", Op: OpSequence, Next: map[string]ExecTarget{
				"then_0": {Node: "latent"},
				"then_1": {Node: "p.sibling"},
			}},
			"latent": {ID: "latent", Op: "test.latent",
				Next: map[string]ExecTarget{"then": {Node: "p.after"}}},
			"p.sibling": printNode("p.sibling", "sibling"),
			"p.after":   printNode("p.after", "after-latent"),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "seq"}}},
	}

	metrics := &fakeExecMetrics{}
	sc := execScene(t, "park-test", prog)
	sc.SetExecMetrics(metrics)
	sc.registerExecOp("test.latent", func(_ *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
		resume, _ := node.next("then")
		return execOpOutcome{park: true, parkKey: "wake-1", resume: resume}
	})
	startScene(t, sc)

	mustFire(t, sc, "e")
	// The sibling runs while the latent child is parked.
	waitForState(t, sc, "__debug.bp.print", `["sibling"]`, time.Second)
	if _, _, parked := metrics.counts(); parked != 1 {
		t.Fatalf("parked gauge = %d, want 1", parked)
	}

	// Resume travels the inbox — the seam the timer wheel (#83) and
	// the authenticated completion path (phase 3) deliver through.
	if !sc.Input(InputMsg{ResumeExec: "wake-1", Source: "test"}) {
		t.Fatal("inbox full on resume")
	}
	waitForState(t, sc, "__debug.bp.print", `["sibling","after-latent"]`, time.Second)
	if _, _, parked := metrics.counts(); parked != 0 {
		t.Fatalf("parked gauge = %d after resume, want 0", parked)
	}

	// A stale/unknown wake key resumes nothing and does not crash.
	sc.Input(InputMsg{ResumeExec: "wake-1", Source: "test"})
	sc.Input(InputMsg{Path: "score.team_a", Value: raw(`1`), Source: "test"})
	waitForState(t, sc, "score.team_a", `1`, time.Second)
}

// TestExec_TimeSlicing_LoopStaysResponsive_NothingKilled: criterion 6.
// A 0..30000 loop is mid-flight when an operator input lands; the
// input→delta latency stays within the 50 ms guarantee because the
// loop drains the inbox between slices — and the task then completes
// with its exact final state: preemption changed scheduling, never
// semantics.
func TestExec_TimeSlicing_LoopStaysResponsive_NothingKilled(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"loop": {ID: "loop", Op: OpForLoop,
				Config: map[string]json.RawMessage{"first": raw(`0`), "last": raw(`30000`)},
				Next: map[string]ExecTarget{
					"body":      {Node: "set"},
					"completed": {Node: "set.done"},
				}},
			"set": varSet("set", "counter",
				[]ExecDataInput{{Port: "value", From: "loop", FromPort: "index"}}, nil),
			"set.done": varSet("set.done", "long_done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "loop"}}},
	}
	prog.Nodes["set.done"].Config["value"] = raw(`true`)

	metrics := &fakeExecMetrics{}
	sc := execScene(t, "slice-test", prog)
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	// No deferred sub.Close(): the scene goroutine outlives the test
	// body (t.Cleanup), and closing the Out channel mid-fanout is a
	// test-side teardown race, not an engine property. The 4096
	// buffer absorbs the remaining emits; Stop tears everything down.
	sub, _ := sc.Subscribe(4096)

	mustFire(t, sc, "e")
	// Give the task time to actually be mid-flight.
	time.Sleep(5 * time.Millisecond)

	start := time.Now()
	if !sc.Input(InputMsg{Path: "score.team_a", Value: raw(`14`), Source: "operator:t", ClientMsgID: "mid-loop"}) {
		t.Fatal("inbox full")
	}
	deadline := time.After(time.Second)
	var latency time.Duration
observe:
	for {
		select {
		case msg := <-sub.Out:
			d, ok := msg.(*protocol.Delta)
			if !ok {
				continue
			}
			for _, p := range d.Patches {
				if p.Path == "score.team_a" && string(p.Value) == `14` {
					latency = time.Since(start)
					break observe
				}
			}
		case <-deadline:
			t.Fatal("input delta never observed while exec task running")
		}
	}
	if latency > 50*time.Millisecond {
		t.Errorf("input-to-delta latency %v exceeds 50ms under exec load (criterion: loop stays reactive)", latency)
	}

	// The long task was preempted — and completed exactly, unkilled.
	waitForState(t, sc, "__vars.bp.long_done", `true`, 5*time.Second)
	waitForState(t, sc, "__vars.bp.counter", `30000`, time.Second)
	if _, preempt, _ := metrics.counts(); preempt == 0 {
		t.Error("expected time-slice preemptions on a 30k-iteration loop")
	}
}

// TestExec_B5_BackPressure_ShedsNewFiresOnly: criterion 17. With the
// per-scene budget saturated by two live `while` tasks, excess NEW
// fires are shed and counted (`orion_event_shed_total`) — and the two
// RUNNING tasks are untouched: once the flag flips they both complete.
func TestExec_B5_BackPressure_ShedsNewFiresOnly(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"while": {ID: "while", Op: OpWhile,
				Data: []ExecDataInput{{Port: "condition", From: "in.run"}},
				Next: map[string]ExecTarget{
					"completed": {Node: "inc.done"},
				}},
			"inc.done": varSet("inc.done", "done",
				[]ExecDataInput{{Port: "value", From: "add.done"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "while"}}},
	}

	metrics := &fakeExecMetrics{}
	sc := execScene(t, "b5-test", prog)
	sc.SetExecMetrics(metrics)
	sc.SetExecBudget(2)
	startScene(t, sc)

	// Four fires in arrival order: two enqueue, two shed (the while
	// tasks are alive until flags.run flips, so the budget is full by
	// construction when fires 3 and 4 are processed).
	for i := 0; i < 4; i++ {
		mustFire(t, sc, "e")
	}

	deadline := time.Now().Add(time.Second)
	for {
		if shed, _, _ := metrics.counts(); shed == 2 {
			break
		}
		if time.Now().After(deadline) {
			shed, _, _ := metrics.counts()
			t.Fatalf("orion_event_shed_total = %d, want 2", shed)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Release the two running tasks: both must complete — back-
	// pressure shed fires, it never killed a task.
	sc.Input(InputMsg{Path: "flags.run", Value: raw(`false`), Source: "test"})
	waitForState(t, sc, "__vars.bp.done", `2`, 5*time.Second)

	if shed, _, _ := metrics.counts(); shed != 2 {
		t.Fatalf("orion_event_shed_total = %d after completion, want exactly 2", shed)
	}
}

// TestExec_B9_VarsIsolationBetweenInstances: criterion 18. Two scene
// instances share the same ExecProgram (same blueprint_key — the
// live + test-session shape): a variable.set in one is visible to
// that instance only; the other's state is bit-identical before and
// after.
func TestExec_B9_VarsIsolationBetweenInstances(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "counter", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "set"}}},
	}
	prog.Nodes["set"].Config["value"] = raw(`42`)

	live := execScene(t, "b9-live", prog)
	session := execScene(t, "b9-session", prog)
	startScene(t, live)
	startScene(t, session)

	_, before := session.state.Snapshot()

	mustFire(t, live, "e")
	waitForState(t, live, "__vars.bp.counter", `42`, time.Second)

	_, after := session.state.Snapshot()
	if len(before) != len(after) {
		t.Fatalf("session state leaf count changed: %d → %d", len(before), len(after))
	}
	for k, v := range before {
		if string(after[k]) != string(v) {
			t.Fatalf("session leaf %s changed: %s → %s (cross-instance __vars bleed)", k, v, after[k])
		}
	}
	if v, _ := session.state.Get("__vars.bp.counter"); string(v) != `0` {
		t.Fatalf("session __vars.bp.counter = %s, want untouched default 0", v)
	}
}

// TestExec_DeterministicInterleaving: the reason continuations beat
// goroutine-per-task (ADR 003 §3.1.7). Two tasks under a pure
// step-budget slice interleave BY CONSTRUCTION in a reproducible
// order: two fresh runs produce byte-identical print rings, and the
// ring shows real interleaving (B lines between A lines), not
// accidental serialization.
func TestExec_DeterministicInterleaving(t *testing.T) {
	buildProg := func() *ExecProgram {
		printNode := func(id, msg string) *ExecNode {
			return &ExecNode{ID: id, Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"` + msg + `"`)}}
		}
		return &ExecProgram{
			BlueprintKey: "bp",
			Nodes: map[string]*ExecNode{
				"loopA": {ID: "loopA", Op: OpForLoop,
					Config: map[string]json.RawMessage{"first": raw(`0`), "last": raw(`3`)},
					Next:   map[string]ExecTarget{"body": {Node: "pA"}}},
				"pA": printNode("pA", "A"),
				"loopB": {ID: "loopB", Op: OpForLoop,
					Config: map[string]json.RawMessage{"first": raw(`0`), "last": raw(`3`)},
					Next:   map[string]ExecTarget{"body": {Node: "pB"}}},
				"pB": printNode("pB", "B"),
			},
			Entrypoints: map[string]ExecEntry{
				"a": {Target: ExecTarget{Node: "loopA"}},
				"b": {Target: ExecTarget{Node: "loopB"}},
			},
		}
	}

	run := func(name string) string {
		sc := execScene(t, name, buildProg())
		// Step-budget-only slicing: wall clock out of the picture.
		sc.SetExecSlicing(5, time.Hour)
		// Enqueue BOTH fires before the loop starts (the inbox is
		// buffered): arrival order is then structural, not a race
		// between the test goroutine's second send and the scene
		// goroutine draining the first — which could serialize task A
		// before B even existed on a slow runner (CI flake).
		mustFire(t, sc, "a")
		mustFire(t, sc, "b")
		startScene(t, sc)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if v, ok := sc.state.Get("__debug.bp.print"); ok {
				var ring []string
				if json.Unmarshal(v, &ring) == nil && len(ring) == 8 {
					return fmt.Sprintf("%v", ring)
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("%s: print ring never reached 8 lines", name)
		return ""
	}

	first := run("det-1")
	second := run("det-2")
	if first != second {
		t.Fatalf("two identical runs interleaved differently:\n  run1 %s\n  run2 %s", first, second)
	}
	// Real interleaving: a B line appears before the last A line.
	if first == "[A A A A B B B B]" {
		t.Fatalf("tasks serialized (%s) — 5-step slices over ~9-step tasks must interleave", first)
	}
}

// TestExec_SingleWriter_UnderConcurrentLoad: the single-writer
// invariant under -race. Fires, operator inputs, snapshots and
// subscriber reads hammer the scene from many goroutines while exec
// tasks run; only the scene goroutine ever writes state — `go test
// -race ./...` fails this test if the exec layer ever leaks a
// concurrent write.
func TestExec_SingleWriter_UnderConcurrentLoad(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"loop": {ID: "loop", Op: OpForLoop,
				Config: map[string]json.RawMessage{"first": raw(`0`), "last": raw(`50`)},
				Next:   map[string]ExecTarget{"body": {Node: "set"}}},
			"set": varSet("set", "counter",
				[]ExecDataInput{{Port: "value", From: "loop", FromPort: "index"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "loop"}}},
	}

	sc := execScene(t, "race-test", prog)
	startScene(t, sc)

	sub, _ := sc.Subscribe(4096)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		n := 0
		for range sub.Out {
			n++ // drain until Close; the count itself is irrelevant
		}
		_ = n
	}()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				sc.FireExec("e", "test")
			}
		}()
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				v, _ := json.Marshal(g*100 + i)
				sc.Input(InputMsg{Path: "score.team_a", Value: v, Source: "test"})
				sc.state.Snapshot()
			}
		}(g)
	}
	wg.Wait()

	// Let in-flight tasks finish, then verify a coherent final state.
	waitForState(t, sc, "__vars.bp.counter", `50`, 5*time.Second)
	// Stop the scene BEFORE closing the subscription: closing the Out
	// channel under a live fan-out is a teardown race, not an engine
	// invariant. Stop is idempotent (t.Cleanup re-runs it harmlessly).
	sc.Stop()
	sub.Close()
	<-drained
}

// TestExec_FireWithoutProgramOrEntry_NoCrash: defensive paths log and
// drop — the scene keeps serving.
func TestExec_FireWithoutProgramOrEntry_NoCrash(t *testing.T) {
	sc := passthroughScene(t, "no-prog")
	startScene(t, sc)
	sc.FireExec("nope", "test")

	sc2 := execScene(t, "no-entry", &ExecProgram{BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{}, Entrypoints: map[string]ExecEntry{}})
	startScene(t, sc2)
	sc2.FireExec("nope", "test")

	sc2.Input(InputMsg{Path: "score.team_a", Value: raw(`3`), Source: "test"})
	waitForState(t, sc2, "score.team_a", `3`, time.Second)
}
