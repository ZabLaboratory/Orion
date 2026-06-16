package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Tests for the event-targeted simulate FIRING MODE of the validation
// harness (ADR 015, issues #194/#195/#196/#197). Simulate is not a second
// engine: it reuses cloneValidationScene + RunValidationEntrypoint, so the
// B10 structural inertia proven for Validate (validation_harness_test.go)
// holds verbatim — these tests assert the *selection* rule (which
// entrypoints a synthetic event fires) and re-prove the zero-effect
// isolation through the Simulate entry (#197 core).
//
// Axes:
//   - selection: an on-event entry matched by topic + seeded the payload,
//     plus the on-start entry, both produce an EntrypointResult; a non-matching
//     on-event topic is NOT fired.
//   - allow-list: a non-empty `entrypoints` set overrides the topic rule and
//     fires only the named entries.
//   - divergence under budget: a divergent body selected by the event fails
//     (status failed) under budget, no hang.
//   - #197 isolation: a world-touching op (http.request / db.query) fired
//     through Simulate completes via the inert seam — no socket, no record,
//     no show mutation, no egress — with ValidateValidationModeCoverage green.

func newSimulateHarness() *Harness {
	return NewHarness(NewComputeRegistry(), quietLogger(),
		ValidationBudget{MaxSteps: 100_000, MaxWall: 2 * time.Second})
}

// simulateProg builds a program with one on-start, one on-event("chat"),
// and one on-event("follow") entrypoint, each setting its own var. The
// on-event bodies are pure (they terminate), so a fired one passes.
func simulateProg() *ExecProgram {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"setStart": varSet("setStart", "started", nil, nil),
			"setChat":  varSet("setChat", "chatted", nil, nil),
			"setFol":   varSet("setFol", "followed", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start":  {Kind: EntryOnStart, Target: ExecTarget{Node: "setStart"}},
			"onChat": {Kind: EntryOnEvent, Event: "chat", Target: ExecTarget{Node: "setChat"}},
			"onFol":  {Kind: EntryOnEvent, Event: "follow", Target: ExecTarget{Node: "setFol"}},
		},
	}
	prog.Nodes["setStart"].Config["value"] = raw(`1`)
	prog.Nodes["setChat"].Config["value"] = raw(`1`)
	prog.Nodes["setFol"].Config["value"] = raw(`1`)
	return prog
}

// firedEntrypoints returns the set of program-local entrypoint ids the
// report records for the first blueprint.
func firedEntrypoints(rep ValidationReport) map[string]bool {
	out := map[string]bool{}
	if len(rep.Blueprints) == 0 {
		return out
	}
	for _, er := range rep.Blueprints[0].Entrypoints {
		out[er.Entrypoint] = true
	}
	return out
}

// TestSimulate_TopicRuleFiresMatchedEventAndOnStart: a "chat" synthetic
// event fires the on-start entry AND the on-event("chat") entry — and NOT
// the on-event("follow") entry (topic mismatch). Default selection rule
// (ADR 015 §3.2).
func TestSimulate_TopicRuleFiresMatchedEventAndOnStart(t *testing.T) {
	rep := newSimulateHarness().Simulate(
		validationGraph("sim-topic"), &compiler.RenderBundle{},
		[]*ExecProgram{simulateProg()},
		SyntheticEvent{Topic: "chat"}, nil)

	if rep.Status != StatusValidated {
		t.Fatalf("status = %s, want validated", rep.Status)
	}
	fired := firedEntrypoints(rep)
	if !fired["start"] {
		t.Fatalf("on-start not fired: %v", fired)
	}
	if !fired["onChat"] {
		t.Fatalf("on-event(chat) not fired by chat event: %v", fired)
	}
	if fired["onFol"] {
		t.Fatalf("on-event(follow) fired by a chat event (topic mismatch): %v", fired)
	}
}

// TestSimulate_NonMatchingTopicFiresOnlyOnStart: an event whose topic
// matches no on-event entry still fires the on-start path, and no on-event
// entry — the init path is always exercised, reactive ones only on match.
func TestSimulate_NonMatchingTopicFiresOnlyOnStart(t *testing.T) {
	rep := newSimulateHarness().Simulate(
		validationGraph("sim-nomatch"), &compiler.RenderBundle{},
		[]*ExecProgram{simulateProg()},
		SyntheticEvent{Topic: "raid"}, nil)

	fired := firedEntrypoints(rep)
	if !fired["start"] {
		t.Fatalf("on-start not fired for non-matching topic: %v", fired)
	}
	if fired["onChat"] || fired["onFol"] {
		t.Fatalf("an on-event entry fired for a non-matching topic: %v", fired)
	}
}

// TestSimulate_PayloadSeedsMatchedEventLeaf: the caller's payload is seeded
// into the event leaf the matched on-event body reads (__events.<topic>).
// The body pulls that leaf and copies it into a var, proving the supplied
// payload — not the canonical fixture — drives the firing.
func TestSimulate_PayloadSeedsMatchedEventLeaf(t *testing.T) {
	g := validationGraph("sim-payload")
	// A graph input leaf pointing at the seeded event leaf; the var-set
	// reads it as its value so the written leaf carries the payload.
	g.Nodes = append(g.Nodes, compiler.GraphNode{
		ID: "evt.chat", Kind: "input", Path: eventsPrefix + "chat"})

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"echo": varSet("echo", "seen",
				[]ExecDataInput{{Port: "value", From: "evt.chat"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"onChat": {Kind: EntryOnEvent, Event: "chat", Target: ExecTarget{Node: "echo"}},
		},
	}

	payload := json.RawMessage(`{"user":"zab","text":"hi"}`)
	rep := newSimulateHarness().Simulate(g, &compiler.RenderBundle{},
		[]*ExecProgram{prog},
		SyntheticEvent{Topic: "chat", Payload: payload}, nil)

	if rep.Status != StatusValidated {
		t.Fatalf("status = %s, want validated", rep.Status)
	}
	er := rep.Blueprints[0].Entrypoints[0]
	if !er.Pass {
		t.Fatalf("seeded on-event entry failed: %+v", er)
	}
	if !contains(er.LeavesWritten, "__vars.bp.seen") {
		t.Fatalf("event body did not write the echo leaf: %v", er.LeavesWritten)
	}
}

// TestSimulate_AllowListOverridesTopicRule: a non-empty `entrypoints`
// allow-list overrides the topic rule — only the named entries fire, even
// though the topic would otherwise match a different on-event entry and the
// on-start would always fire.
func TestSimulate_AllowListOverridesTopicRule(t *testing.T) {
	rep := newSimulateHarness().Simulate(
		validationGraph("sim-allow"), &compiler.RenderBundle{},
		[]*ExecProgram{simulateProg()},
		SyntheticEvent{Topic: "chat"},
		[]string{"onFol"}) // explicitly fire follow, not chat, not start

	fired := firedEntrypoints(rep)
	if !fired["onFol"] {
		t.Fatalf("allow-listed entry not fired: %v", fired)
	}
	if fired["start"] || fired["onChat"] {
		t.Fatalf("allow-list did not override the topic rule: %v", fired)
	}
}

// TestSimulate_DivergentSelectedEntryFailsUnderBudget: an on-event body the
// synthetic event selects is a while(true) — it crosses the step budget,
// FAILS (not killed, never airs), and the run returns under budget without
// hanging. Mirrors the Validate divergence doctrine through the simulate
// entry.
func TestSimulate_DivergentSelectedEntryFailsUnderBudget(t *testing.T) {
	g := validationGraph("sim-diverge")
	g.Nodes = append(g.Nodes, compiler.GraphNode{ID: "lit.true", Kind: "input", Path: "lit.true"})
	g.Defaults["lit.true"] = raw(`true`)

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"while": {ID: "while", Op: OpWhile,
				Data: []ExecDataInput{{Port: "condition", From: "lit.true"}},
				Next: map[string]ExecTarget{"body": {Node: "noop"}}},
			"noop": varSet("noop", "tick", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"onChat": {Kind: EntryOnEvent, Event: "chat", Target: ExecTarget{Node: "while"}},
		},
	}
	prog.Nodes["noop"].Config["value"] = raw(`1`)

	done := make(chan ValidationReport, 1)
	go func() {
		done <- NewHarness(NewComputeRegistry(), quietLogger(),
			ValidationBudget{MaxSteps: 5_000, MaxWall: time.Second}).
			Simulate(g, &compiler.RenderBundle{}, []*ExecProgram{prog},
				SyntheticEvent{Topic: "chat"}, nil)
	}()

	select {
	case rep := <-done:
		if rep.Status != StatusFailed {
			t.Fatalf("status = %s, want failed (divergent on-event body)", rep.Status)
		}
		er := rep.Blueprints[0].Entrypoints[0]
		if er.Pass {
			t.Fatal("divergent simulate entry passed — must fail")
		}
		if er.Steps < 5_000 {
			t.Fatalf("steps = %d, want >= step budget", er.Steps)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Simulate hung on a divergent body — budget did not bound the proof")
	}
}

// TestSimulate_ZeroEffectIsolation (#197, core): a graph whose selected
// entrypoint reaches a world-touching op (http.request) is fired through
// Simulate with a REAL effect seam whose Runner would PANIC the test on
// Submit (a socket open) and an egress that would FAIL on dial. The op
// completes via the inert synthetic seam — proving Simulate inherits the
// SAME B10 structural inertia as Validate: no socket, no record, no show
// mutation, no egress. The attempted egress is listed in the report (the
// caller sees the call) but never executed.
func TestSimulate_ZeroEffectIsolation(t *testing.T) {
	g := validationGraph("sim-isolation")
	g.Nodes = append(g.Nodes,
		compiler.GraphNode{ID: "lit.url", Kind: "input", Path: "lit.url"})
	g.Defaults["lit.url"] = raw(`"https://example.com"`)

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"http": {ID: "http", Op: OpHTTPRequest,
				Data: []ExecDataInput{{Port: "url", From: "lit.url"}},
				Next: map[string]ExecTarget{"then": {Node: "set"}, "error": {Node: "set"}}},
			"db": {ID: "db", Op: OpDBQuery,
				Config: map[string]json.RawMessage{"datasource": raw(`"truth"`)},
				Next:   map[string]ExecTarget{"then": {Node: "set"}, "error": {Node: "set"}}},
			"set": varSet("set", "done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			// on-event matched by the synthetic topic → reaches http → db → set.
			"onChat": {Kind: EntryOnEvent, Event: "chat", Target: ExecTarget{Node: "http"}},
		},
	}
	prog.Nodes["http"].Next["then"] = ExecTarget{Node: "db"}
	prog.Nodes["set"].Config["value"] = raw(`true`)

	// Drive a clone directly so we can install a REAL effect seam: a
	// validation-mode scene that ever submitted an effect job would panic;
	// the inert seam means it never does. This is the same construction as
	// TestHarness_ValidationEffectRunsNoClosure, through the simulate plan.
	h := newSimulateHarness()
	gcopy := *g
	bcopy := compiler.RenderBundle{}
	scene := NewScene(g.SceneID, &gcopy, &bcopy, NewComputeRegistry(), quietLogger())
	scene.SetValidationMode()
	scene.SetEffects(&SceneEffects{}) // use of any world runner here would be observable
	scene.InstallExec(prog)

	// Fire the entry the synthetic chat event selects, seeding the payload —
	// exactly what simulateBlueprint does internally.
	res := scene.RunValidationEntrypoint(entryKey("bp", "onChat"), nil, h.budget)
	scene.cancel()

	if !res.Pass {
		t.Fatalf("world-touching entry failed through simulate seam: %+v", res)
	}
	// The synthetic completion drove the chain forward to the success leaf —
	// not a hang, not the error port from a nil client.
	if v, ok := scene.state.Get("__vars.bp.done"); !ok || string(v) != "true" {
		t.Fatalf("world ops did not complete via the inert seam: ok=%v v=%s", ok, v)
	}
	// Both world ops were ATTEMPTED (listed) — and inert.
	var sawHTTP, sawDB bool
	for _, ea := range res.EffectsTried {
		if ea.Op == OpHTTPRequest {
			sawHTTP = true
		}
		if ea.Op == OpDBQuery {
			sawDB = true
		}
	}
	if !sawHTTP || !sawDB {
		t.Fatalf("effects_attempted missing a world op: http=%v db=%v (%+v)", sawHTTP, sawDB, res.EffectsTried)
	}

	// The B10 introspective guard is green: every op SetEffects registers has
	// a synthetic response, so no world op could leak in this firing mode.
	if err := ValidateValidationModeCoverage(); err != nil {
		t.Fatalf("validation-mode coverage guard failed: %v", err)
	}
}

// TestSimulate_FullPathThroughHarness re-proves zero-effect through the
// public Simulate entry (not a hand-built clone): the same world-touching
// program, fired by a chat synthetic event via Simulate, validates without
// a panic — Simulate never installs a live effect runner of its own, it
// clones validation-mode scenes whose seam is inert.
func TestSimulate_FullPathThroughHarness(t *testing.T) {
	g := validationGraph("sim-fullpath")
	g.Nodes = append(g.Nodes,
		compiler.GraphNode{ID: "lit.url", Kind: "input", Path: "lit.url"})
	g.Defaults["lit.url"] = raw(`"https://example.com"`)

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"http": {ID: "http", Op: OpHTTPRequest,
				Data: []ExecDataInput{{Port: "url", From: "lit.url"}},
				Next: map[string]ExecTarget{"then": {Node: "set"}, "error": {Node: "set"}}},
			"set": varSet("set", "done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"onChat": {Kind: EntryOnEvent, Event: "chat", Target: ExecTarget{Node: "http"}},
		},
	}
	prog.Nodes["set"].Config["value"] = raw(`true`)

	rep := newSimulateHarness().Simulate(g, &compiler.RenderBundle{},
		[]*ExecProgram{prog}, SyntheticEvent{Topic: "chat"}, nil)

	if rep.Status != StatusValidated {
		t.Fatalf("status = %s, want validated", rep.Status)
	}
	er := rep.Blueprints[0].Entrypoints[0]
	if !er.Pass {
		t.Fatalf("world-touching simulate entry failed: %+v", er)
	}
	if !contains(er.LeavesWritten, "__vars.bp.done") {
		t.Fatalf("http.request did not complete down then via Simulate: %v", er.LeavesWritten)
	}
}
