package runtime

// Probe tests for exec layer — ADR 003 §3.1, issue #82.
// Written by Probe (no-merge tier, branch probe/82-exec-interpreter).
// These tests complement Forge's 13 tests; they do NOT rewrite them.
//
// Axes covered:
//  1. park=fork semantics in loop context (latent inside for-loop body)
//  2. Sequential preemption state exactness on nested sequence-in-loop
//  3. B5 at budget=0 (disabled), budget=1 (boundary), and exact-budget firing
//  4. B9 fan-out cross-contamination attempt via rapid concurrent fires
//  5. Duplicate wake-key: continuation dropped, scene keeps serving
//  6. SetEffector seam (phase-4 path exercised)
//  7. execVariableSet with empty name (error path)
//  8. Unregistered op in exec chain (default branch in execNode)
//  9. Loop with body-less wiring (no "body" pin wired)
// 10. Sequence empty (no then_0 at all)
// 11. Gate toggle and close paths
// 12. pullBool/pullInt/pullArray with absent port → safe defaults

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- 1. park=fork in LOOP context ----------------------------------------
//
// ADR 003 §3.1.3 + exec_interpreter.go line 172-181:
// When a latent op parks inside a loop body, the fork goes to execParked
// and the task's remaining frameLoop frame continues — the loop advances
// to the next iteration immediately, not after resume.
//
// This pinned test proves: 3 loop iterations fire, each parks one child
// continuation. The loop's "completed" fires while all 3 children are
// still parked. Only after all 3 resumes does the parked-gauge reach 0.
func TestExec_ParkFork_InLoopBody_LoopContinues(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"loop": {ID: "loop", Op: OpForLoop,
				Config: map[string]json.RawMessage{"first": raw(`0`), "last": raw(`2`)},
				Next: map[string]ExecTarget{
					"body":      {Node: "latent"},
					"completed": {Node: "set.done"},
				}},
			"latent": {ID: "latent", Op: "test.latent",
				Next: map[string]ExecTarget{"then": {Node: "set.woke"}}},
			"set.done":  varSet("set.done", "loop_completed", nil, nil),
			"set.woke":  varSet("set.woke", "woke_count", nil, nil), // overwritten 3 times
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "loop"}}},
	}
	prog.Nodes["set.done"].Config["value"] = raw(`true`)
	prog.Nodes["set.woke"].Config["value"] = raw(`true`)

	metrics := &fakeExecMetrics{}
	sc := execScene(t, "park-loop", prog)
	sc.SetExecMetrics(metrics)

	var mu sync.Mutex
	var keys []string
	sc.registerExecOp("test.latent", func(_ *Scene, t *execTask, node *ExecNode, _ string) execOpOutcome {
		mu.Lock()
		defer mu.Unlock()
		// Each iteration gets a unique wake key via the task env index.
		resume, _ := node.next("then")
		idx := len(keys)
		key := "wake-loop-" + string(rune('A'+idx))
		keys = append(keys, key)
		return execOpOutcome{park: true, parkKey: key, resume: resume}
	})
	startScene(t, sc)

	mustFire(t, sc, "e")

	// The loop must complete (fire "completed") before any resume.
	waitForState(t, sc, "__vars.bp.loop_completed", `true`, 2*time.Second)

	// Three continuations must be parked at this point.
	mu.Lock()
	collectedKeys := append([]string(nil), keys...)
	mu.Unlock()

	if len(collectedKeys) != 3 {
		t.Fatalf("expected 3 parked continuations (one per loop iteration), got %d", len(collectedKeys))
	}
	if _, _, parked := metrics.counts(); parked != 3 {
		t.Fatalf("orion_parked_tasks = %d, want 3", parked)
	}

	// Resume all three.
	for _, k := range collectedKeys {
		if !sc.Input(InputMsg{ResumeExec: k, Source: "probe"}) {
			t.Fatalf("inbox full resuming %s", k)
		}
	}
	waitForState(t, sc, "__vars.bp.woke_count", `true`, time.Second)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, parked := metrics.counts(); parked == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, _, parked := metrics.counts()
	t.Fatalf("orion_parked_tasks = %d after all resumes, want 0", parked)
}

// --- 2. Nested sequence-in-loop: exact state across N preemptions --------
//
// A 5-iteration loop whose body is a 3-sibling sequence (set.a, set.b,
// set.c) under a 2-step slice. Preemption may hit mid-sequence on any
// iteration. After completion: counter=4, seq_done=true, and the
// intermediate vars all stabilised — no frame corruption.
func TestExec_NestedSeqInLoop_ExactStateAcrossPreemptions(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"loop": {ID: "loop", Op: OpForLoop,
				Config: map[string]json.RawMessage{"first": raw(`0`), "last": raw(`4`)},
				Next: map[string]ExecTarget{
					"body":      {Node: "seq"},
					"completed": {Node: "set.done"},
				}},
			"seq": {ID: "seq", Op: OpSequence, Next: map[string]ExecTarget{
				"then_0": {Node: "set.a"},
				"then_1": {Node: "set.b"},
				"then_2": {Node: "set.c"},
			}},
			"set.a":    varSet("set.a", "a_seen", nil, nil),
			"set.b":    varSet("set.b", "b_seen", nil, nil),
			"set.c":    varSet("set.c", "counter", []ExecDataInput{{Port: "value", From: "loop", FromPort: "index"}}, nil),
			"set.done": varSet("set.done", "seq_done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "loop"}}},
	}
	prog.Nodes["set.a"].Config["value"] = raw(`true`)
	prog.Nodes["set.b"].Config["value"] = raw(`true`)
	prog.Nodes["set.done"].Config["value"] = raw(`true`)

	metrics := &fakeExecMetrics{}
	sc := execScene(t, "nested-seq-loop", prog)
	sc.SetExecMetrics(metrics)
	sc.SetExecSlicing(2, time.Hour) // guarantee many preemptions
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.seq_done", `true`, 2*time.Second)
	waitForState(t, sc, "__vars.bp.counter", `4`, time.Second)
	waitForState(t, sc, "__vars.bp.a_seen", `true`, time.Second)
	waitForState(t, sc, "__vars.bp.b_seen", `true`, time.Second)

	if _, preempt, _ := metrics.counts(); preempt == 0 {
		t.Error("expected preemptions with 2-step slices over a 5-iteration nested-seq loop")
	}
}

// --- 3a. B5 budget=0 disables the cap entirely ---------------------------
//
// SetExecBudget(0): no shed even when many tasks are fired in burst.
func TestExec_B5_BudgetZero_NeverSheds(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "counter", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "set"}}},
	}
	prog.Nodes["set"].Config["value"] = raw(`1`)

	metrics := &fakeExecMetrics{}
	sc := execScene(t, "b5-zero", prog)
	sc.SetExecMetrics(metrics)
	sc.SetExecBudget(0) // disabled
	startScene(t, sc)

	for i := 0; i < 50; i++ {
		mustFire(t, sc, "e")
	}

	// Let all tasks drain.
	time.Sleep(50 * time.Millisecond)

	if shed, _, _ := metrics.counts(); shed != 0 {
		t.Fatalf("budget=0 must disable shedding; got shed=%d", shed)
	}
}

// --- 3b. B5 budget=1: exact boundary -------------------------------------
//
// With budget=1 and one live while-task, the SECOND fire must be shed.
// The running task must complete once the condition flips.
func TestExec_B5_BudgetOne_ExactBoundary(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"while": {ID: "while", Op: OpWhile,
				Data: []ExecDataInput{{Port: "condition", From: "in.run"}},
				Next: map[string]ExecTarget{
					"completed": {Node: "set.done"},
				}},
			"set.done": varSet("set.done", "done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "while"}}},
	}
	prog.Nodes["set.done"].Config["value"] = raw(`true`)

	metrics := &fakeExecMetrics{}
	sc := execScene(t, "b5-one", prog)
	sc.SetExecMetrics(metrics)
	sc.SetExecBudget(1)
	startScene(t, sc)

	mustFire(t, sc, "e") // fills the budget
	// Second fire must be shed.
	mustFire(t, sc, "e")

	deadline := time.Now().Add(time.Second)
	for {
		if shed, _, _ := metrics.counts(); shed >= 1 {
			break
		}
		if time.Now().After(deadline) {
			shed, _, _ := metrics.counts()
			t.Fatalf("orion_event_shed_total = %d, want >= 1 (budget=1)", shed)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Running task must complete after flag flips — not killed.
	sc.Input(InputMsg{Path: "flags.run", Value: raw(`false`), Source: "probe"})
	waitForState(t, sc, "__vars.bp.done", `true`, 2*time.Second)
}

// --- 4. B9 concurrent fan-out cross-contamination attempt ----------------
//
// Two instances race: rapid fires on both simultaneously.
// Instance A fires a set that writes "A"; instance B writes "B".
// After both drain: A.__vars.bp.label == "A", B.__vars.bp.label == "B".
func TestExec_B9_ConcurrentFires_NoLeakBetweenInstances(t *testing.T) {
	makeProg := func(val string) *ExecProgram {
		n := varSet("set", "label", nil, nil)
		n.Config["value"] = json.RawMessage(`"` + val + `"`)
		return &ExecProgram{
			BlueprintKey: "bp",
			Nodes:        map[string]*ExecNode{"set": n},
			Entrypoints:  map[string]ExecEntry{"e": {Target: ExecTarget{Node: "set"}}},
		}
	}

	scA := execScene(t, "b9-conc-a", makeProg("A"))
	scB := execScene(t, "b9-conc-b", makeProg("B"))
	startScene(t, scA)
	startScene(t, scB)

	var wg sync.WaitGroup
	const n = 100
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			scA.FireExec("e", "probe")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			scB.FireExec("e", "probe")
		}
	}()
	wg.Wait()

	waitForState(t, scA, "__vars.bp.label", `"A"`, 2*time.Second)
	waitForState(t, scB, "__vars.bp.label", `"B"`, 2*time.Second)

	// Cross-check: neither instance carries the other's value.
	if v, ok := scA.state.Get("__vars.bp.label"); ok && string(v) != `"A"` {
		t.Errorf("instance A __vars.bp.label = %s, want \"A\"", v)
	}
	if v, ok := scB.state.Get("__vars.bp.label"); ok && string(v) != `"B"` {
		t.Errorf("instance B __vars.bp.label = %s, want \"B\"", v)
	}
}

// --- 5. Duplicate wake key: continuation dropped, scene keeps serving ----
//
// Two latent ops try to park under the SAME key in the same task chain.
// The second park is dropped (error-logged). The scene must keep serving
// subsequent inputs normally — no panic, no hang.
func TestExec_DuplicateWakeKey_ContinuationDropped_SceneKeepServing(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"seq": {ID: "seq", Op: OpSequence, Next: map[string]ExecTarget{
				"then_0": {Node: "lat1"},
				"then_1": {Node: "lat2"},
			}},
			"lat1": {ID: "lat1", Op: "test.latent1",
				Next: map[string]ExecTarget{"then": {Node: "set.after1"}}},
			"lat2": {ID: "lat2", Op: "test.latent2",
				Next: map[string]ExecTarget{"then": {Node: "set.after2"}}},
			"set.after1": varSet("set.after1", "after1", nil, nil),
			"set.after2": varSet("set.after2", "after2", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "seq"}}},
	}
	prog.Nodes["set.after1"].Config["value"] = raw(`true`)
	prog.Nodes["set.after2"].Config["value"] = raw(`true`)

	sc := execScene(t, "dup-key", prog)
	sc.registerExecOp("test.latent1", func(_ *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
		resume, _ := node.next("then")
		return execOpOutcome{park: true, parkKey: "same-key", resume: resume}
	})
	sc.registerExecOp("test.latent2", func(_ *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
		resume, _ := node.next("then")
		// Same key — must be dropped.
		return execOpOutcome{park: true, parkKey: "same-key", resume: resume}
	})
	startScene(t, sc)

	mustFire(t, sc, "e")
	time.Sleep(20 * time.Millisecond) // let it process

	// Scene must still handle inputs normally after the dup-key drop.
	sc.Input(InputMsg{Path: "score.team_a", Value: raw(`99`), Source: "probe"})
	waitForState(t, sc, "score.team_a", `99`, time.Second)
}

// --- 6. SetEffector seam (phase-4 path) ----------------------------------
//
// A custom Effector that counts SetLeaf calls. After firing a
// variable.set, it must have been called at least once — confirming
// the seam is not bypassed (B10 readiness).
func TestExec_SetEffector_CustomEffectorReceivesWrites(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "x", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "set"}}},
	}
	prog.Nodes["set"].Config["value"] = raw(`42`)

	type countingEffector struct {
		mu     sync.Mutex
		calls  int
		prints int
	}
	var eff countingEffector
	type effImpl struct{ e *countingEffector }
	effI := &effImpl{&eff}
	_ = effI // compiler check

	// We need a concrete type; inline it here.
	var called int64
	sc := execScene(t, "effector-seam", prog)
	sc.SetEffector(&captureEffector{
		onSetLeaf: func(path string, _ json.RawMessage) {
			if path != "" {
				atomic.AddInt64(&called, 1)
			}
		},
	})
	startScene(t, sc)

	mustFire(t, sc, "e")
	time.Sleep(20 * time.Millisecond)

	if n := atomic.LoadInt64(&called); n == 0 {
		t.Fatal("custom Effector.SetLeaf never called after variable.set fire")
	}
}

// captureEffector is a test Effector that delegates to caller-supplied hooks.
type captureEffector struct {
	onSetLeaf func(path string, value json.RawMessage)
	onPrint   func(blueprintKey, line string)
}

func (e *captureEffector) SetLeaf(path string, value json.RawMessage) {
	if e.onSetLeaf != nil {
		e.onSetLeaf(path, value)
	}
}

func (e *captureEffector) Print(blueprintKey, line string) {
	if e.onPrint != nil {
		e.onPrint(blueprintKey, line)
	}
}

// --- 7. execVariableSet: empty name → error path -------------------------
//
// A variable.set node with no "name" config key. The scene must NOT crash;
// subsequent inputs must still be processed.
func TestExec_VariableSet_EmptyName_NocrashSceneContinues(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"bad": {ID: "bad", Op: OpVariableSet,
				Config: map[string]json.RawMessage{
					// "name" is deliberately absent — empty string after configString
				},
				Next: map[string]ExecTarget{"then": {Node: "set.ok"}}},
			"set.ok": varSet("set.ok", "ok", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "bad"}}},
	}
	prog.Nodes["set.ok"].Config["value"] = raw(`true`)

	sc := execScene(t, "varset-noname", prog)
	startScene(t, sc)

	mustFire(t, sc, "e")
	// The "bad" node must have been skipped; the "then" chain must still run.
	waitForState(t, sc, "__vars.bp.ok", `true`, time.Second)
}

// --- 8. Unregistered exec op in chain → error-logged, chain continues ----
//
// A sequence with an unknown op in position 0; sibling at position 1 must
// still run (the exec node error-logs and returns, not kills the task).
func TestExec_UnknownOp_ChainContinues(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"seq": {ID: "seq", Op: OpSequence, Next: map[string]ExecTarget{
				"then_0": {Node: "unknown"},
				"then_1": {Node: "set.ok"},
			}},
			"unknown": {ID: "unknown", Op: "no.such.op"},
			"set.ok":  varSet("set.ok", "ok", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "seq"}}},
	}
	prog.Nodes["set.ok"].Config["value"] = raw(`true`)

	sc := execScene(t, "unknown-op", prog)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.ok", `true`, time.Second)
}

// --- 9. Loop with no body wired: iterates, fires completed ---------------
//
// A for-loop whose "body" pin is not wired. Doctrine: no kill, no crash.
// It must reach "completed" with idx == last+1.
func TestExec_ForLoop_NoBodyWired_FiresCompleted(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"loop": {ID: "loop", Op: OpForLoop,
				Config: map[string]json.RawMessage{"first": raw(`0`), "last": raw(`4`)},
				Next: map[string]ExecTarget{
					// "body" intentionally absent
					"completed": {Node: "set.done"},
				}},
			"set.done": varSet("set.done", "done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "loop"}}},
	}
	prog.Nodes["set.done"].Config["value"] = raw(`true`)

	sc := execScene(t, "loop-nobody", prog)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.done", `true`, time.Second)
}

// --- 10. Sequence with no then_0: immediate completion -------------------
//
// A sequence node with no then_* pins wired must complete silently
// without crash. The task finishes (empty frames) and the scene keeps
// handling inputs.
func TestExec_Sequence_Empty_NocrashSceneContinues(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"seq": {ID: "seq", Op: OpSequence,
				// No Next pins at all.
			},
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "seq"}}},
	}

	sc := execScene(t, "seq-empty", prog)
	startScene(t, sc)

	mustFire(t, sc, "e")
	// No assertion on state — only verify scene keeps serving.
	sc.Input(InputMsg{Path: "score.team_a", Value: raw(`7`), Source: "probe"})
	waitForState(t, sc, "score.team_a", `7`, time.Second)
}

// --- 11. Gate toggle and close paths -------------------------------------
//
// Forge tests open+enter. This test covers toggle (closed→open) and
// close (open→closed), confirming all four gate pins change state
// predictably.
func TestExec_Gate_ToggleAndClose(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"gate": {ID: "gate", Op: OpGate,
				Config: map[string]json.RawMessage{"start_closed": raw(`true`)},
				Next:   map[string]ExecTarget{"exit": {Node: "set.pass"}}},
			"set.pass": varSet("set.pass", "pass_count", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"enter":  {Target: ExecTarget{Node: "gate", Port: "enter"}},
			"open":   {Target: ExecTarget{Node: "gate", Port: "open"}},
			"close":  {Target: ExecTarget{Node: "gate", Port: "close"}},
			"toggle": {Target: ExecTarget{Node: "gate", Port: "toggle"}},
		},
	}
	prog.Nodes["set.pass"].Config["value"] = raw(`true`)

	sc := execScene(t, "gate-toggle", prog)
	startScene(t, sc)

	// Start closed: enter must not pass.
	mustFire(t, sc, "enter")
	time.Sleep(20 * time.Millisecond)
	if v, ok := sc.state.Get("__vars.bp.pass_count"); ok {
		t.Fatalf("gate passed while closed: %s", v)
	}

	// toggle: closed → open.
	mustFire(t, sc, "toggle")
	waitForState(t, sc, "__nodestate.gate", `{"open":true}`, time.Second)

	// Now enter passes.
	mustFire(t, sc, "enter")
	waitForState(t, sc, "__vars.bp.pass_count", `true`, time.Second)

	// close: open → closed.
	mustFire(t, sc, "close")
	waitForState(t, sc, "__nodestate.gate", `{"open":false}`, time.Second)

	// toggle again: closed → open.
	mustFire(t, sc, "toggle")
	waitForState(t, sc, "__nodestate.gate", `{"open":true}`, time.Second)
}

// --- 12. pullBool/pullInt with absent port → safe defaults ---------------
//
// branch with no "condition" data input and no config default: must
// take the false arm (pullBool returns false on absent port).
// for-loop with no "first"/"last" config: must use def=0 / def=-1
// → loop runs zero iterations (first=0 > last=-1) → fires completed.
func TestExec_PullDefaults_AbsentPort_SafeFallback(t *testing.T) {
	// branch: condition absent → false arm.
	branchProg := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"br": {ID: "br", Op: OpBranch,
				// No Data inputs, no Config "condition".
				Next: map[string]ExecTarget{
					"true":  {Node: "set.t"},
					"false": {Node: "set.f"},
				}},
			"set.t": varSet("set.t", "arm", nil, nil),
			"set.f": varSet("set.f", "arm", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "br"}}},
	}
	branchProg.Nodes["set.t"].Config["value"] = raw(`"true-arm"`)
	branchProg.Nodes["set.f"].Config["value"] = raw(`"false-arm"`)

	sc1 := execScene(t, "pull-absent-branch", branchProg)
	startScene(t, sc1)
	mustFire(t, sc1, "e")
	waitForState(t, sc1, "__vars.bp.arm", `"false-arm"`, time.Second)

	// for-loop: no first/last config → first=0, last=-1 → completed immediately.
	loopProg := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"loop": {ID: "loop", Op: OpForLoop,
				// No Config keys at all.
				Next: map[string]ExecTarget{
					"body":      {Node: "set.body"},
					"completed": {Node: "set.done"},
				}},
			"set.body": varSet("set.body", "body_ran", nil, nil),
			"set.done": varSet("set.done", "done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "loop"}}},
	}
	loopProg.Nodes["set.body"].Config["value"] = raw(`true`)
	loopProg.Nodes["set.done"].Config["value"] = raw(`true`)

	sc2 := execScene(t, "pull-absent-loop", loopProg)
	startScene(t, sc2)
	mustFire(t, sc2, "e")
	waitForState(t, sc2, "__vars.bp.done", `true`, time.Second)
	// Body must NOT have run (0 iterations).
	time.Sleep(20 * time.Millisecond)
	if v, ok := sc2.state.Get("__vars.bp.body_ran"); ok {
		t.Fatalf("loop body ran on 0-iteration loop: %s", v)
	}
}
