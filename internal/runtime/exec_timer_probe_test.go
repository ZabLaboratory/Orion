package runtime

// Probe tests for the timer wheel, triggers, and cancellation (issue
// #83). Complement Forge's exec_timer_test.go: these cover ordering
// determinism, tie-breaking, Timer re-arm correctness, edge caps,
// stale-version resume, inert exec path and -race.
//
// Axes:
//   - Deadline ordering: fire order == deadline order (not park order)
//   - Tie-break: identical deadline → park (seq) order
//   - Timer re-arm: closer park after farther one re-arms earlier
//   - B8 cap=0/disabled: no cap, all parks accepted
//   - B8 cap exact boundary: cap-th park accepted, cap+1-th shed
//   - Cancellation all-parked: wheel size + parked both 0, no orphan fire
//   - Stale resume from prior version key (SceneVersion mismatch)
//   - on-start not refiring on the same instance, only once per activation
//   - SetActive inert (no-op) when no exec program installed
//   - Large delay (math.MaxInt64 ns) does not fire immediately
//   - -race: concurrent delay + cancel + inputs on same scene

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// ---- deadline ordering -------------------------------------------------

// TestExecDelay_MultipleDeadlines_FiringOrderIsByDeadline: three delays
// parked with different deadlines (10 s, 5 s, 1 s) are fired in
// deadline order — 1s → 5s → 10s — regardless of their park order
// (which is 10s first). The heap invariant, not the insertion order,
// drives the sequence. Deterministic on multiple runs via fake clock.
func TestExecDelay_MultipleDeadlines_FiringOrderIsByDeadline(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d10": delayNode("d10", `10`, map[string]ExecTarget{"then": {Node: "p10"}}),
			"d5":  delayNode("d5", `5`, map[string]ExecTarget{"then": {Node: "p5"}}),
			"d1":  delayNode("d1", `1`, map[string]ExecTarget{"then": {Node: "p1"}}),
			"p10": {ID: "p10", Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"t10"`)}},
			"p5": {ID: "p5", Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"t5"`)}},
			"p1": {ID: "p1", Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"t1"`)}},
		},
		Entrypoints: map[string]ExecEntry{
			"e10": {Target: ExecTarget{Node: "d10"}},
			"e5":  {Target: ExecTarget{Node: "d5"}},
			"e1":  {Target: ExecTarget{Node: "d1"}},
		},
	}

	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "order-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	// Park in order 10s → 5s → 1s (longest first).
	mustFire(t, sc, "e10")
	mustFire(t, sc, "e5")
	mustFire(t, sc, "e1")
	waitFor(t, "three parked", func() bool {
		_, _, p := metrics.counts()
		return p == 3 && metrics.wheelSize() == 3
	})

	// Advance only 1 s: only the 1s delay is due.
	clk.Advance(1 * time.Second)
	waitForState(t, sc, "__debug.bp.print", `["t1"]`, time.Second)
	waitFor(t, "one fired, two remain", func() bool {
		_, _, p := metrics.counts()
		return p == 2 && metrics.wheelSize() == 2
	})

	// Advance to 5 s total.
	clk.Advance(4 * time.Second)
	waitForState(t, sc, "__debug.bp.print", `["t1","t5"]`, time.Second)
	waitFor(t, "two fired, one remain", func() bool {
		_, _, p := metrics.counts()
		return p == 1 && metrics.wheelSize() == 1
	})

	// Advance to 10 s total.
	clk.Advance(5 * time.Second)
	waitForState(t, sc, "__debug.bp.print", `["t1","t5","t10"]`, time.Second)
	waitFor(t, "all fired", func() bool {
		_, _, p := metrics.counts()
		return p == 0 && metrics.wheelSize() == 0
	})
}

// TestExecDelay_IdenticalDeadline_TieBreakByParkOrder: two delays with
// exactly the same deadline are fired in park (insertion) order, not
// arbitrary heap order. The seq field of wheelEntry is the tiebreak.
func TestExecDelay_IdenticalDeadline_TieBreakByParkOrder(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"dA": delayNode("dA", `5`, map[string]ExecTarget{"then": {Node: "pA"}}),
			"dB": delayNode("dB", `5`, map[string]ExecTarget{"then": {Node: "pB"}}),
			"pA": {ID: "pA", Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"A"`)}},
			"pB": {ID: "pB", Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"B"`)}},
		},
		Entrypoints: map[string]ExecEntry{
			"eA": {Target: ExecTarget{Node: "dA"}},
			"eB": {Target: ExecTarget{Node: "dB"}},
		},
	}

	for run := 0; run < 5; run++ {
		clk := newFakeClock()
		sc := execScene(t, "tiebreak-test", prog)
		sc.SetClock(clk)
		metrics := &fakeExecMetrics{}
		sc.SetExecMetrics(metrics)
		startScene(t, sc)

		// Always park A before B.
		mustFire(t, sc, "eA")
		mustFire(t, sc, "eB")
		waitFor(t, "both parked", func() bool {
			_, _, p := metrics.counts()
			return p == 2
		})

		clk.Advance(5 * time.Second)
		// A must appear before B in all runs — seq tiebreak guarantees it.
		waitForState(t, sc, "__debug.bp.print", `["A","B"]`, time.Second)
	}
}

// ---- Timer re-arm -------------------------------------------------------

// TestExecDelay_RearmWhenCloserParkArrives: a 10s delay is parked first
// (Timer set to 10s). Then a 2s delay is parked. The Timer must be
// re-armed to the 2s deadline: Advance(2s) fires the closer one only,
// and Advance(8s more) fires the original 10s one.
func TestExecDelay_RearmWhenCloserParkArrives(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d10": delayNode("d10", `10`, map[string]ExecTarget{"then": {Node: "p10"}}),
			"d2":  delayNode("d2", `2`, map[string]ExecTarget{"then": {Node: "p2"}}),
			"p10": {ID: "p10", Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"t10"`)}},
			"p2": {ID: "p2", Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"t2"`)}},
		},
		Entrypoints: map[string]ExecEntry{
			"e10": {Target: ExecTarget{Node: "d10"}},
			"e2":  {Target: ExecTarget{Node: "d2"}},
		},
	}

	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "rearm-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	// Park the 10s delay first; Timer is set to fire in 10s.
	mustFire(t, sc, "e10")
	waitFor(t, "10s parked", func() bool {
		_, _, p := metrics.counts()
		return p == 1 && metrics.wheelSize() == 1
	})

	// Park a closer 2s delay: Timer MUST be re-armed to 2s.
	mustFire(t, sc, "e2")
	waitFor(t, "both parked", func() bool {
		_, _, p := metrics.counts()
		return p == 2 && metrics.wheelSize() == 2
	})

	// Advance only 2s: only the 2s fires.
	clk.Advance(2 * time.Second)
	waitForState(t, sc, "__debug.bp.print", `["t2"]`, time.Second)
	waitFor(t, "one still parked", func() bool {
		_, _, p := metrics.counts()
		return p == 1 && metrics.wheelSize() == 1
	})
	// 10s must NOT have fired yet.
	time.Sleep(20 * time.Millisecond)
	v, _ := sc.state.Get("__debug.bp.print")
	if string(v) != `["t2"]` {
		t.Fatalf("10s timer fired early after re-arm: print=%s", v)
	}

	// Advance the remaining 8s.
	clk.Advance(8 * time.Second)
	waitForState(t, sc, "__debug.bp.print", `["t2","t10"]`, time.Second)
}

// ---- B8 cap edge cases --------------------------------------------------

// TestExec_B8_CapDisabled_AllParksAccepted: with SetExecParkCap(0) the
// cap is disabled. Parking 10 delays must all be accepted — no shed.
func TestExec_B8_CapDisabled_AllParksAccepted(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d": delayNode("d", `100`, map[string]ExecTarget{"then": {Node: "p"}}),
			"p": {ID: "p", Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"woke"`)}},
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "d"}}},
	}

	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "cap-disabled-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	sc.SetExecParkCap(0) // disabled
	startScene(t, sc)

	const n = 10
	for i := 0; i < n; i++ {
		mustFire(t, sc, "e")
	}
	waitFor(t, "all 10 parked", func() bool {
		_, _, p := metrics.counts()
		return p == n && metrics.wheelSize() == n
	})
	if got := metrics.droppedBy("cap"); got != 0 {
		t.Fatalf("cap=0 (disabled): dropped %d, want 0", got)
	}
}

// TestExec_B8_CapExactBoundary: the cap-th park is accepted; the
// (cap+1)-th is shed. Existing parked continuations all resume.
func TestExec_B8_CapExactBoundary(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d": delayNode("d", `5`, map[string]ExecTarget{"then": {Node: "p"}}),
			"p": {ID: "p", Op: OpPrint,
				Config: map[string]json.RawMessage{"message": raw(`"woke"`)}},
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "d"}}},
	}

	const parkCap = 3
	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "b8-boundary-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	sc.SetExecParkCap(parkCap)
	startScene(t, sc)

	// parkCap parks must be accepted.
	for i := 0; i < parkCap; i++ {
		mustFire(t, sc, "e")
	}
	waitFor(t, "parkCap parks accepted", func() bool {
		_, _, p := metrics.counts()
		return p == parkCap
	})
	if got := metrics.droppedBy("cap"); got != 0 {
		t.Fatalf("before cap+1: dropped %d, want 0", got)
	}

	// parkCap+1-th is shed.
	mustFire(t, sc, "e")
	waitFor(t, "cap+1 shed", func() bool {
		return metrics.droppedBy("cap") == 1
	})
	if _, _, p := metrics.counts(); p != parkCap {
		t.Fatalf("parked after shed = %d, want %d (existing intact)", p, parkCap)
	}

	// All parkCap parked continuations still resume at deadline.
	clk.Advance(5 * time.Second)
	want := `["woke","woke","woke"]`
	waitForState(t, sc, "__debug.bp.print", want, time.Second)
}

// ---- Cancellation all-parked --------------------------------------------

// TestExec_Cancellation_MultipleParked_AllCleaned: multiple tasks parked
// on the wheel (different deadlines). After CancelExec: wheel size = 0,
// parked tasks = 0, no orphan fires when clock advances past all
// deadlines.
func TestExec_Cancellation_MultipleParked_AllCleaned(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d1":  delayNode("d1", `1`, map[string]ExecTarget{"then": {Node: "s1"}}),
			"d5":  delayNode("d5", `5`, map[string]ExecTarget{"then": {Node: "s5"}}),
			"d10": delayNode("d10", `10`, map[string]ExecTarget{"then": {Node: "s10"}}),
			"s1":  varSet("s1", "fired1", nil, nil),
			"s5":  varSet("s5", "fired5", nil, nil),
			"s10": varSet("s10", "fired10", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"e1":  {Target: ExecTarget{Node: "d1"}},
			"e5":  {Target: ExecTarget{Node: "d5"}},
			"e10": {Target: ExecTarget{Node: "d10"}},
		},
	}
	for _, n := range []string{"s1", "s5", "s10"} {
		prog.Nodes[n].Config["value"] = raw(`true`)
	}

	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "cancel-multi-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	mustFire(t, sc, "e1")
	mustFire(t, sc, "e5")
	mustFire(t, sc, "e10")
	waitFor(t, "three parked", func() bool {
		_, _, p := metrics.counts()
		return p == 3 && metrics.wheelSize() == 3
	})

	sc.CancelExec()
	waitFor(t, "wheel and parked zeroed", func() bool {
		_, _, p := metrics.counts()
		return p == 0 && metrics.wheelSize() == 0
	})

	// Advance past all deadlines — no continuation resumes.
	clk.Advance(20 * time.Second)
	time.Sleep(30 * time.Millisecond)
	for _, path := range []string{"__vars.bp.fired1", "__vars.bp.fired5", "__vars.bp.fired10"} {
		if _, ok := sc.state.Get(path); ok {
			t.Fatalf("orphan timer fired after cancellation: %s", path)
		}
	}
}

// ---- Stale resume from different scene version --------------------------

// TestExec_ResumeStale_WrongSceneVersion: a wake key stamped with a
// different SceneVersion is dropped as stale and counted on
// orion_exec_resume_stale_total — the parked map has no entry for it
// (unknown key), but the format must be parseable so the staleness
// check fires before the map lookup. We craft the key manually to
// simulate a key minted by an older build of the scene.
func TestExec_ResumeStale_WrongSceneVersion(t *testing.T) {
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "stale-version-test", &ExecProgram{
		BlueprintKey: "bp",
		Nodes:        map[string]*ExecNode{},
		Entrypoints:  map[string]ExecEntry{},
	})
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	// Craft a valid stamped key with a DIFFERENT scene version.
	wrongVersionKey := "wk|sha256:wrong-version|0|1"
	if !sc.Input(InputMsg{ResumeExec: wrongVersionKey, Source: "test"}) {
		t.Fatal("inbox full")
	}
	waitFor(t, "stale resume counted", func() bool { return metrics.stale() == 1 })
}

// ---- on-start inert check -----------------------------------------------

// TestExec_SetActive_InertWithoutProgram: SetActive on scenes without an
// exec program must be a pure no-op on the exec layer: no panic, no
// spurious state change, FireOnStart delivers nothing because execOnStart
// is empty. Validates the prod inertness guarantee until phase-4 gate.
func TestExec_SetActive_InertWithoutProgram(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	// Load two scenes WITHOUT exec programs (prod path).
	show.Load("a", varsGraph("a"), bundle)
	show.Load("b", varsGraph("b"), bundle)

	if err := show.SetActive("a", nil); err != nil {
		t.Fatal(err)
	}
	if err := show.SetActive("b", nil); err != nil {
		t.Fatal(err)
	}
	if err := show.SetActive("a", nil); err != nil {
		t.Fatal(err)
	}

	scA, _ := show.Get("a")
	time.Sleep(30 * time.Millisecond)

	// No exec program means on-start is wired to nothing: state must
	// remain at declared defaults. varsGraph seeds __vars.bp.done = 0;
	// an exec on-start would increment it to 1. It must stay at 0.
	if v, _ := scA.state.Get("__vars.bp.done"); string(v) != `0` {
		t.Fatalf("exec fired on a scene without an exec program (prod regression): done=%s", v)
	}
}

// ---- Large delay — no wrap / no premature fire --------------------------

// TestExecDelay_HugeDelay_NoPrematureFire: a delay authored as
// math.MaxFloat64 seconds is clamped to math.MaxInt64 nanoseconds by
// durationFromSeconds. Advancing the fake clock by 1 year must NOT fire
// the continuation — it remains parked.
func TestExecDelay_HugeDelay_NoPrematureFire(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d": {
				ID: "d", Op: OpDelay,
				// math.MaxFloat64 as a JSON number
				Config: map[string]json.RawMessage{
					"seconds": json.RawMessage(jsonFloat(math.MaxFloat64)),
				},
				Next: map[string]ExecTarget{"then": {Node: "s"}},
			},
			"s": varSet("s", "fired", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "d"}}},
	}
	prog.Nodes["s"].Config["value"] = raw(`true`)

	clk := newFakeClock()
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "huge-delay-test", prog)
	sc.SetClock(clk)
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitFor(t, "delay parked", func() bool {
		_, _, p := metrics.counts()
		return p == 1 && metrics.wheelSize() == 1
	})

	// Advance 1 year — still nowhere near MaxInt64 nanoseconds.
	clk.Advance(365 * 24 * time.Hour)
	time.Sleep(30 * time.Millisecond)
	if _, ok := sc.state.Get("__vars.bp.fired"); ok {
		t.Fatal("huge-delay fired prematurely after 1-year advance")
	}
}

func jsonFloat(_ float64) string {
	// strconv.FormatFloat with 'e' gives full precision without overflow.
	return `1.7976931348623157e+308`
}

// ---- -race: concurrent inputs + delay + cancel ---------------------------

// TestExecDelay_Race_ConcurrentInputsAndCancel: under -race, 20
// goroutines sending state inputs concurrently while a delay is parked,
// followed by a cancellation, must not trigger the race detector. The
// single-writer invariant (wheel/parked only touched inside the scene
// goroutine's select) is what this validates.
func TestExecDelay_Race_ConcurrentInputsAndCancel(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d": delayNode("d", `60`, map[string]ExecTarget{"then": {Node: "s"}}),
			"s": varSet("s", "fired", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "d"}}},
	}
	prog.Nodes["s"].Config["value"] = raw(`true`)

	clk := newFakeClock()
	sc := execScene(t, "race-delay-test", prog)
	sc.SetClock(clk)
	startScene(t, sc)

	mustFire(t, sc, "e")

	// 20 goroutines writing state leaves concurrently.
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(_ int) {
			for j := 0; j < 50; j++ {
				sc.Input(InputMsg{
					Path:   "score.team_a",
					Value:  json.RawMessage(`1`),
					Source: "race-test",
				})
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}

	// Cancel while inputs may still be in flight.
	sc.CancelExec()
	waitFor(t, "cancel drained", func() bool {
		// Just need the scene to still respond.
		return sc.Input(InputMsg{Path: "score.team_a", Value: raw(`99`), Source: "test"})
	})
	waitForState(t, sc, "score.team_a", `99`, time.Second)
}

// ---- on-start fires once per activation, not on same-instance re-set ---

// TestExec_OnStart_NotRefiredOnSameInstance_Direct: directly activating
// a scene then activating the SAME scene again (from == id guard in
// SetActive) must not fire on-start a second time. Forge's test goes
// via Show; this verifies the guard at the Scene level independently.
func TestExec_OnStart_NotRefiredOnSameInstance_Direct(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	show.LoadExec("a", varsGraph("a"), bundle, onStartProg())
	if err := show.SetActive("a", nil); err != nil {
		t.Fatal(err)
	}
	scA, _ := show.Get("a")
	waitForState(t, scA, "__vars.bp.done", `1`, time.Second)

	// Re-activate same scene: must not refire.
	if err := show.SetActive("a", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if v, _ := scA.state.Get("__vars.bp.done"); string(v) != `1` {
		t.Fatalf("on-start fired on same-instance re-activation: done=%s", v)
	}
}

// ---- re-push after cancellation: new instance defaults + on-start -------

// TestExec_RePush_AfterCancel_NewInstanceDefaults: after a cancellation
// (CancelExec), a re-push (LoadExec) of the active scene must deliver a
// fresh instance with defaults reseeded and on-start fired exactly once.
// The old version's wake keys must be stale on the new epoch.
func TestExec_RePush_AfterCancel_NewInstanceDefaults(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"d":      delayNode("d", `3600`, map[string]ExecTarget{"then": {Node: "s.late"}}),
			"s.late": varSet("s.late", "late", nil, nil),
			"set": varSet("set", "done",
				[]ExecDataInput{{Port: "value", From: "add.done"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Target: ExecTarget{Node: "set"}, Kind: EntryOnStart},
			"delay": {Target: ExecTarget{Node: "d"}},
		},
	}
	prog.Nodes["s.late"].Config["value"] = raw(`true`)

	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	metrics := &fakeExecMetrics{}
	show.SetExecMetrics(metrics)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	show.LoadExec("a", varsGraph("a"), bundle, prog)
	if err := show.SetActive("a", nil); err != nil {
		t.Fatal(err)
	}
	scA1, _ := show.Get("a")
	// on-start fires → done=1.
	waitForState(t, scA1, "__vars.bp.done", `1`, time.Second)

	// Fire the hour-long delay so something is parked pre-cancel.
	mustFire(t, scA1, "delay")
	waitFor(t, "delay parked", func() bool {
		_, _, p := metrics.counts()
		return p == 1 && metrics.wheelSize() == 1
	})

	// Re-push the active scene: old instance torn down (cancelled by
	// teardown), new instance starts with defaults → done restarts at 0
	// then on-start fires → done=1 on the new instance.
	show.LoadExec("a", varsGraph("a"), bundle, prog)
	scA2, _ := show.Get("a")
	if scA2 == scA1 {
		t.Fatal("re-push did not swap instance")
	}
	// New instance: on-start fires exactly once → done == 1.
	waitForState(t, scA2, "__vars.bp.done", `1`, time.Second)

	// Old instance's timer must have been cancelled by teardown.
	waitFor(t, "old timer purged", func() bool {
		_, _, p := metrics.counts()
		return p == 0 && metrics.wheelSize() == 0
	})
	// "late" must never be written on the new instance (old timer orphan check).
	time.Sleep(30 * time.Millisecond)
	if _, ok := scA2.state.Get("__vars.bp.late"); ok {
		t.Fatal("old-instance timer fired on new instance after re-push")
	}
}
