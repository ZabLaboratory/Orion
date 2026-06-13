package runtime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Tests for the timer wheel, `delay`, the triggers and cancellation
// (ADR 003 §3.1.3/§3.1.4, issue #83). All timing-sensitive semantics
// run on a FAKE clock: no real sleeps decide a deadline, ever — the
// few time.Sleep calls below only give the scene goroutine a beat for
// NEGATIVE assertions (X must not have happened).

// fakeClock is a deterministic Clock for tests. Advance moves time
// and fires every due timer; a timer armed with d <= 0 fires
// immediately (matching time.NewTimer(0)). Mirrors the go>=1.23 Timer
// contract: no delivery after Stop, Reset supersedes any pending
// delivery.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clk: c, c: make(chan time.Time, 1)}
	t.armLocked(d)
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the fake instant by d and delivers every due timer.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		t.fireIfDueLocked(c.now)
	}
}

type fakeTimer struct {
	clk      *fakeClock
	c        chan time.Time
	deadline time.Time
	active   bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) armLocked(d time.Duration) {
	t.deadline = t.clk.now.Add(d)
	t.active = true
	t.fireIfDueLocked(t.clk.now)
}

func (t *fakeTimer) fireIfDueLocked(now time.Time) {
	if !t.active || t.deadline.After(now) {
		return
	}
	t.active = false
	select {
	case t.c <- now:
	default:
	}
}

func (t *fakeTimer) Stop() {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	t.active = false
	select { // go>=1.23 contract: no delivery survives Stop
	case <-t.c:
	default:
	}
}

func (t *fakeTimer) Reset(d time.Duration) {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	select { // Reset supersedes any pending delivery
	case <-t.c:
	default:
	}
	t.armLocked(d)
}

// waitFor polls cond until true or fails the test.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}

// stateAbsent asserts the leaf was never written (negative assertion:
// give the goroutine a beat first).
func stateAbsent(t *testing.T, sc *Scene, path string) {
	t.Helper()
	time.Sleep(25 * time.Millisecond)
	if v, ok := sc.state.Get(path); ok {
		t.Fatalf("leaf %s = %s, want absent", path, v)
	}
}

func delayNode(id string, seconds string, next map[string]ExecTarget) *ExecNode {
	return &ExecNode{
		ID: id, Op: OpDelay,
		Config: map[string]json.RawMessage{"seconds": raw(seconds)},
		Next:   next,
	}
}

// TestExecDelay_FiresAtDeadline_FakeClock: criterion 3 — `delay`
// fires `then` at +seconds, on the fake clock. Before the deadline
// nothing resumes; at the deadline the continuation resumes on the
// scene goroutine. `orion_timer_wheel_size` and `orion_parked_tasks`
// track the parked continuation.
func TestExecDelay_FiresAtDeadline_FakeClock(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d":   delayNode("d", `2.5`, map[string]ExecTarget{"then": {Node: "set"}}),
			"set": varSet("set", "fired", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "d"}}},
	}
	prog.Nodes["set"].Config["value"] = raw(`true`)

	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "delay-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitFor(t, "delay parked on the wheel", func() bool {
		_, _, parked := metrics.counts()
		return parked == 1 && metrics.wheelSize() == 1
	})

	clk.Advance(1 * time.Second) // not due yet
	stateAbsent(t, sc, "__vars.bp.fired")

	clk.Advance(1500 * time.Millisecond) // exactly the deadline
	waitForState(t, sc, "__vars.bp.fired", `true`, time.Second)
	waitFor(t, "wheel and parked drained", func() bool {
		_, _, parked := metrics.counts()
		return parked == 0 && metrics.wheelSize() == 0
	})
}

// TestExecDelay_InLoop_ParkIsFork: the park=fork semantics confirmed
// by #82, on a real `delay`. The enclosing for-loop COMPLETES while
// its three suspended chains wait; at the deadline they resume in
// strict park order (same deadline → wheel seq tiebreak), each with
// its own per-iteration `index` snapshot — deterministic, never
// map-ordered.
func TestExecDelay_InLoop_ParkIsFork(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"loop": {ID: "loop", Op: OpForLoop,
				Config: map[string]json.RawMessage{"first": raw(`0`), "last": raw(`2`)},
				Next: map[string]ExecTarget{
					"body":      {Node: "d"},
					"completed": {Node: "p.done"},
				}},
			"d": delayNode("d", `5`, map[string]ExecTarget{"then": {Node: "p.idx"}}),
			"p.idx": {ID: "p.idx", Op: OpPrint,
				Data: []ExecDataInput{{Port: "value", From: "loop", FromPort: "index"}}},
			"p.done": {ID: "p.done", Op: OpPrint,
				Config: map[string]json.RawMessage{"value": raw(`"loop-done"`)}},
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "loop"}}},
	}

	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "delay-loop-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	mustFire(t, sc, "e")
	// The loop finishes (prints "loop-done") while all three delayed
	// chains are still parked: park forked, the loop never waited.
	waitForState(t, sc, "__debug.bp.print", `["loop-done"]`, time.Second)
	waitFor(t, "three parked timer continuations", func() bool {
		_, _, parked := metrics.counts()
		return parked == 3 && metrics.wheelSize() == 3
	})

	clk.Advance(5 * time.Second)
	// Each continuation kept ITS iteration's index (env snapshot at
	// fork), resumed in park order.
	waitForState(t, sc, "__debug.bp.print", `["loop-done","0","1","2"]`, time.Second)
	waitFor(t, "wheel drained after firing", func() bool {
		_, _, parked := metrics.counts()
		return parked == 0 && metrics.wheelSize() == 0
	})
}

// TestExecDelay_NegativeAndMinusZero_FireImmediately: the IEEE-754
// `-0` / negative-seconds trap. A delay of `-0`, a negative delay and
// a zero delay all have the same DEFINED behaviour: the chain still
// parks (latent semantics — the fork happens) and the timer fires
// immediately, with no Advance, no panic and no infinite wait.
func TestExecDelay_NegativeAndMinusZero_FireImmediately(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d.negzero": delayNode("d.negzero", `-0`, map[string]ExecTarget{"then": {Node: "s.negzero"}}),
			"d.neg":     delayNode("d.neg", `-3`, map[string]ExecTarget{"then": {Node: "s.neg"}}),
			"d.zero":    delayNode("d.zero", `0`, map[string]ExecTarget{"then": {Node: "s.zero"}}),
			"s.negzero": varSet("s.negzero", "negzero", nil, nil),
			"s.neg":     varSet("s.neg", "neg", nil, nil),
			"s.zero":    varSet("s.zero", "zero", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"negzero": {Target: ExecTarget{Node: "d.negzero"}},
			"neg":     {Target: ExecTarget{Node: "d.neg"}},
			"zero":    {Target: ExecTarget{Node: "d.zero"}},
		},
	}
	prog.Nodes["s.negzero"].Config["value"] = raw(`true`)
	prog.Nodes["s.neg"].Config["value"] = raw(`true`)
	prog.Nodes["s.zero"].Config["value"] = raw(`true`)

	clk := newFakeClock()
	sc := execScene(t, "delay-neg-test", prog)
	sc.SetClock(clk)
	startScene(t, sc)

	mustFire(t, sc, "negzero")
	mustFire(t, sc, "neg")
	mustFire(t, sc, "zero")
	// No Advance at all: a non-positive deadline is already due.
	waitForState(t, sc, "__vars.bp.negzero", `true`, time.Second)
	waitForState(t, sc, "__vars.bp.neg", `true`, time.Second)
	waitForState(t, sc, "__vars.bp.zero", `true`, time.Second)
}

// onStartProg increments `__vars.bp.done` on each on-start fire (the
// add reads the CURRENT counter, so refires are observable).
func onStartProg() *ExecProgram {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "done",
				[]ExecDataInput{{Port: "value", From: "add.done"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Target: ExecTarget{Node: "set"}, Kind: EntryOnStart},
		},
	}
	return prog
}

// TestExec_OnStart_TestSessionOpen: `on-start` fires on each
// test-session open (ADR 003 §3.1.3) — the pre-gate substrate where
// exec actually runs (R9).
func TestExec_OnStart_TestSessionOpen(t *testing.T) {
	mgr := NewTestSessionManager(NewComputeRegistry(), quietLogger(), time.Minute)
	t.Cleanup(mgr.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	_, sc := mgr.Open(ctx, "ts-scene", varsGraph("ts-scene"),
		&compiler.RenderBundle{SceneVersion: "sha256:exec-test"}, onStartProg())
	waitForState(t, sc, "__vars.bp.done", `1`, time.Second)
}

// TestExec_OnStart_ActivationAndRePush: `on-start` fires when the
// scene becomes live (SetActive) and again on a push-swap of the
// active scene — where the NEW instance starts from declared defaults
// (restart-reseed + on-start, ADR 003 §3.1.4). Loading a non-active
// scene fires nothing; re-activating the already-active scene REFIRES
// on-start without reseeding state (ADR 008 Amendment 1 §A1.2 — activation
// is the canonical (re)launch verb; refire ≠ reseed §A1.5).
func TestExec_OnStart_ActivationAndRePush(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	show.LoadExec("a", varsGraph("a"), bundle, onStartProg())
	scA, _ := show.Get("a")
	// Loaded but not live: no on-start — the counter keeps its
	// declared default 0.
	time.Sleep(25 * time.Millisecond)
	if v, _ := scA.state.Get("__vars.bp.done"); string(v) != `0` {
		t.Fatalf("on-start fired on Load of a non-active scene: done=%s", v)
	}

	if err := show.SetActive("a", nil); err != nil {
		t.Fatal(err)
	}
	waitForState(t, scA, "__vars.bp.done", `1`, time.Second)

	// Re-activating the already-active scene REFIRES on-start (ADR 008
	// Amendment 1 §A1.2). The SAME instance is reused (no reseed): the
	// add-from-current reads the preserved 1 and writes 2 — refire observed,
	// state preserved. A reseed would have snapped done back to 0 then 1.
	if err := show.SetActive("a", nil); err != nil {
		t.Fatal(err)
	}
	waitForState(t, scA, "__vars.bp.done", `2`, time.Second)
	if scA2, _ := show.Get("a"); scA2 != scA {
		t.Fatal("re-activation must reuse the same instance (refire, not reseed)")
	}

	// Push-swap of the ACTIVE scene: fresh instance, defaults
	// reseeded, on-start fired exactly once → done == 1 (not 2:
	// the counter restarted from the default 0).
	show.LoadExec("a", varsGraph("a"), bundle, onStartProg())
	scA2, _ := show.Get("a")
	if scA2 == scA {
		t.Fatal("re-push did not swap the scene instance")
	}
	waitForState(t, scA2, "__vars.bp.done", `1`, time.Second)
}

// TestExec_OnTick_DeltaSeconds: `on-tick` fires per global-tick write
// with `delta_seconds` bound under the event node's id — 0 on the
// first observed tick, then the diff between tick instants.
func TestExec_OnTick_DeltaSeconds(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "delta",
				[]ExecDataInput{{Port: "value", From: "tickn", FromPort: "delta_seconds"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"tick": {Target: ExecTarget{Node: "set"}, Kind: EntryOnTick, Node: "tickn"},
		},
	}
	sc := execScene(t, "tick-test", prog)
	startScene(t, sc)

	sc.Input(InputMsg{Path: tickPath, Value: raw(`5000`), Source: "system:tick", IsSystem: true})
	waitForState(t, sc, "__vars.bp.delta", `0`, time.Second)

	sc.Input(InputMsg{Path: tickPath, Value: raw(`5250`), Source: "system:tick", IsSystem: true})
	waitForState(t, sc, "__vars.bp.delta", `0.25`, time.Second)
}

// TestExec_OnEvent_FiresPerWrite: `on-event` fires on each write to
// `__events.<event>` — including a write carrying the SAME payload
// (an event is an event, not a value change), and the fired task
// observes the payload through the leaf.
func TestExec_OnEvent_FiresPerWrite(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "done",
				[]ExecDataInput{{Port: "value", From: "add.done"}},
				map[string]ExecTarget{"then": {Node: "set.payload"}}),
			"set.payload": varSet("set.payload", "payload",
				[]ExecDataInput{{Port: "value", From: "__events.goal"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"goal":  {Target: ExecTarget{Node: "set"}, Kind: EntryOnEvent, Event: "goal"},
			"other": {Target: ExecTarget{Node: "set"}, Kind: EntryOnEvent, Event: "other"},
		},
	}
	sc := execScene(t, "event-test", prog)
	startScene(t, sc)

	sc.Input(InputMsg{Path: "__events.goal", Value: raw(`{"team":"a"}`), Source: "operator:x"})
	waitForState(t, sc, "__vars.bp.done", `1`, time.Second)
	waitForState(t, sc, "__vars.bp.payload", `{"team":"a"}`, time.Second)

	// Same payload again: state.Set is a no-op, the trigger is not.
	sc.Input(InputMsg{Path: "__events.goal", Value: raw(`{"team":"a"}`), Source: "operator:x"})
	waitForState(t, sc, "__vars.bp.done", `2`, time.Second)

	// A write to an unrelated leaf fires nothing.
	sc.Input(InputMsg{Path: "score.team_a", Value: raw(`7`), Source: "operator:x"})
	waitForState(t, sc, "score.team_a", `7`, time.Second)
	if v, _ := sc.state.Get("__vars.bp.done"); string(v) != `2` {
		t.Fatalf("non-event write fired on-event: done=%s", v)
	}
}

// TestExec_Cancellation_MidDelay_TimersPurgedStaleResumeCounted:
// criterion 7 on the timer path. Cancellation mid-delay drops the
// parked continuation and purges the wheel; advancing the clock past
// the original deadline resumes NOTHING; and a resume carrying a
// wake key minted before the cancellation (version/epoch-stamped) is
// dropped + counted (`orion_exec_resume_stale_total`), resuming
// nothing.
func TestExec_Cancellation_MidDelay_TimersPurgedStaleResumeCounted(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d":      delayNode("d", `5`, map[string]ExecTarget{"then": {Node: "s.late"}}),
			"s.late": varSet("s.late", "late", nil, nil),
			"stamped": {ID: "stamped", Op: "test.stamped",
				Next: map[string]ExecTarget{"then": {Node: "s.resumed"}}},
			"s.resumed": varSet("s.resumed", "resumed", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"e":       {Target: ExecTarget{Node: "d"}},
			"stamped": {Target: ExecTarget{Node: "stamped"}},
		},
	}
	prog.Nodes["s.late"].Config["value"] = raw(`true`)
	prog.Nodes["s.resumed"].Config["value"] = raw(`true`)

	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	keyCh := make(chan string, 1)
	sc := execScene(t, "cancel-delay-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	// A synthetic latent op that parks under a freshly minted
	// version-stamped key and reports it — the handle the stale-resume
	// assertion needs.
	sc.registerExecOp("test.stamped", func(s *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
		key := s.nextWakeKey()
		keyCh <- key
		resume, _ := node.next("then")
		return execOpOutcome{park: true, parkKey: key, resume: resume}
	})
	startScene(t, sc)

	mustFire(t, sc, "e")
	mustFire(t, sc, "stamped")
	var preCancelKey string
	select {
	case preCancelKey = <-keyCh:
	case <-time.After(time.Second):
		t.Fatal("stamped op never ran")
	}
	waitFor(t, "delay + stamped op parked", func() bool {
		_, _, parked := metrics.counts()
		return parked == 2 && metrics.wheelSize() == 1
	})

	// §3.1.4: cancel all live tasks of this scene version.
	sc.CancelExec()
	waitFor(t, "cancellation drained parked + wheel", func() bool {
		_, _, parked := metrics.counts()
		return parked == 0 && metrics.wheelSize() == 0
	})

	// Past the original deadline: the purged timer resumes nothing.
	clk.Advance(10 * time.Second)
	stateAbsent(t, sc, "__vars.bp.late")

	// The in-flight result of the cancelled version arrives late: the
	// stale stamp (old epoch) drops it, counted, resuming nothing.
	if !sc.Input(InputMsg{ResumeExec: preCancelKey, Source: "test"}) {
		t.Fatal("inbox full on stale resume")
	}
	waitFor(t, "stale resume counted", func() bool { return metrics.stale() == 1 })
	stateAbsent(t, sc, "__vars.bp.resumed")

	// The scene keeps serving after cancellation.
	sc.Input(InputMsg{Path: "score.team_a", Value: raw(`3`), Source: "test"})
	waitForState(t, sc, "score.team_a", `3`, time.Second)
}

// TestExec_Cancellation_MidLoop: criterion 7 on the runnable path. A
// live `while` task (incrementing a counter every iteration) is
// dropped by cancellation: the counter stops advancing, `completed`
// never fires, and the scene stays fully responsive.
func TestExec_Cancellation_MidLoop(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"while": {ID: "while", Op: OpWhile,
				Data: []ExecDataInput{{Port: "condition", From: "in.run"}},
				Next: map[string]ExecTarget{
					"body":      {Node: "inc"},
					"completed": {Node: "s.done"},
				}},
			"inc": varSet("inc", "counter",
				[]ExecDataInput{{Port: "value", From: "add.counter"}}, nil),
			"s.done": varSet("s.done", "while_done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "while"}}},
	}
	prog.Nodes["s.done"].Config["value"] = raw(`true`)

	sc := execScene(t, "cancel-loop-test", prog)
	sc.SetExecSlicing(20, time.Hour)
	startScene(t, sc)

	mustFire(t, sc, "e")
	// The loop is observably running (counter advancing).
	waitFor(t, "loop mid-flight", func() bool {
		v, ok := sc.state.Get("__vars.bp.counter")
		return ok && string(v) != `0`
	})

	sc.CancelExec()
	// After the cancel drains, the counter freezes.
	var frozen string
	waitFor(t, "counter frozen after cancel", func() bool {
		v, _ := sc.state.Get("__vars.bp.counter")
		if frozen == string(v) && frozen != "" {
			return true
		}
		frozen = string(v)
		time.Sleep(10 * time.Millisecond)
		return false
	})
	time.Sleep(25 * time.Millisecond)
	if v, _ := sc.state.Get("__vars.bp.counter"); string(v) != frozen {
		t.Fatalf("cancelled loop still running: %s → %s", frozen, v)
	}
	stateAbsent(t, sc, "__vars.bp.while_done")

	// Still serving.
	sc.Input(InputMsg{Path: "score.team_a", Value: raw(`9`), Source: "test"})
	waitForState(t, sc, "score.team_a", `9`, time.Second)
}

// TestExec_Cancellation_SwitchAway: §3.1.4 through the Show — the
// operator switching away cancels the previous scene's live tasks
// (parked continuation dropped, timer purged); the previous scene
// itself keeps running its dataflow.
func TestExec_Cancellation_SwitchAway(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d":      delayNode("d", `3600`, map[string]ExecTarget{"then": {Node: "s.late"}}),
			"s.late": varSet("s.late", "late", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Target: ExecTarget{Node: "d"}, Kind: EntryOnStart},
		},
	}
	prog.Nodes["s.late"].Config["value"] = raw(`true`)

	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	metrics := &fakeExecMetrics{}
	show.SetExecMetrics(metrics)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	show.LoadExec("a", varsGraph("a"), bundle, prog)
	show.Load("b", varsGraph("b"), bundle)

	if err := show.SetActive("a", nil); err != nil {
		t.Fatal(err)
	}
	// on-start fired the hour-long delay: parked on the wheel.
	waitFor(t, "scene a parked mid-delay", func() bool {
		_, _, parked := metrics.counts()
		return parked == 1 && metrics.wheelSize() == 1
	})

	// Switch away: scene a's tasks are cancelled (ADR 003 §3.1.4).
	if err := show.SetActive("b", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "switch-away cancelled scene a's tasks", func() bool {
		_, _, parked := metrics.counts()
		return parked == 0 && metrics.wheelSize() == 0
	})

	// Scene a still serves dataflow after the cancellation.
	scA, _ := show.Get("a")
	scA.Input(InputMsg{Path: "score.team_a", Value: raw(`4`), Source: "test"})
	waitForState(t, scA, "score.team_a", `4`, time.Second)
	if _, ok := scA.state.Get("__vars.bp.late"); ok {
		t.Fatal("cancelled delay fired anyway")
	}
}

// TestExec_B8_ParkCap_ShedsNewParkOnly: criterion 17, parked-cap half.
// With the per-scene parked cap full, a NEW park is shed and counted
// (`orion_exec_park_dropped_total{reason="cap"}`) — and the tasks
// already parked are untouched: both resume at their deadline. No
// task is ever killed; the shedding task itself continues (its
// surrounding frames keep running).
func TestExec_B8_ParkCap_ShedsNewParkOnly(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d": delayNode("d", `5`, map[string]ExecTarget{"then": {Node: "p"}}),
			"p": {ID: "p", Op: OpPrint,
				Config: map[string]json.RawMessage{"value": raw(`"woke"`)}},
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "d"}}},
	}

	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "b8-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	sc.SetExecParkCap(2)
	startScene(t, sc)

	for i := 0; i < 3; i++ {
		mustFire(t, sc, "e")
	}
	waitFor(t, "two parks accepted, third shed", func() bool {
		_, _, parked := metrics.counts()
		return parked == 2 && metrics.droppedBy("cap") == 1
	})

	// The two EXISTING parked continuations are intact: both fire.
	clk.Advance(5 * time.Second)
	waitForState(t, sc, "__debug.bp.print", `["woke","woke"]`, time.Second)
	if got := metrics.droppedBy("cap"); got != 1 {
		t.Fatalf("cap sheds = %d, want exactly 1", got)
	}
}

// TestExec_C1_DuplicateWakeKeyCounted: the C1 counter (Bastion
// condition inherited from #82). A wake-key collision drops the new
// continuation, counted on
// `orion_exec_park_dropped_total{reason="duplicate_key"}` — and the
// originally parked continuation still resumes correctly.
func TestExec_C1_DuplicateWakeKeyCounted(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"latent": {ID: "latent", Op: "test.fixedkey",
				Next: map[string]ExecTarget{"then": {Node: "set"}}},
			"set": varSet("set", "done",
				[]ExecDataInput{{Port: "value", From: "add.done"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "latent"}}},
	}

	metrics := &fakeExecMetrics{}
	sc := execScene(t, "c1-test", prog)
	sc.SetExecMetrics(metrics)
	sc.registerExecOp("test.fixedkey", func(_ *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
		resume, _ := node.next("then")
		return execOpOutcome{park: true, parkKey: "dup-key", resume: resume}
	})
	startScene(t, sc)

	mustFire(t, sc, "e") // parks under "dup-key"
	mustFire(t, sc, "e") // collides — dropped + counted
	waitFor(t, "duplicate wake key counted", func() bool {
		return metrics.droppedBy("duplicate_key") == 1
	})
	if _, _, parked := metrics.counts(); parked != 1 {
		t.Fatalf("parked = %d, want 1 (original continuation intact)", parked)
	}

	// The original continuation resumes exactly once.
	if !sc.Input(InputMsg{ResumeExec: "dup-key", Source: "test"}) {
		t.Fatal("inbox full on resume")
	}
	waitForState(t, sc, "__vars.bp.done", `1`, time.Second)
}
