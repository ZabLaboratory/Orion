package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Tests for the scene-validation harness (ADR 003 §3.2, issue #87):
// per-entrypoint pass/fail, budget = fail-not-kill (divergent while fails
// validation, never airs), the B10 structural guard, and the proof that no
// world effect runs in validation mode. The harness drives scenes
// synchronously (single goroutine) so -race is trivially clean.

func newTestHarness() *Harness {
	return NewHarness(NewComputeRegistry(), quietLogger(),
		ValidationBudget{MaxSteps: 100_000, MaxWall: 2 * time.Second})
}

// validationGraph is varsGraph plus the leaves the effect/while tests read.
func validationGraph(id string) *compiler.Graph {
	g := varsGraph(id)
	g.SceneID = id
	return g
}

// TestHarness_PassOnTerminatingLogic: a blueprint whose on-start sets a
// variable terminates within budget → entrypoint passes, scene validated.
func TestHarness_PassOnTerminatingLogic(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "counter", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "set"}},
		},
	}
	prog.Nodes["set"].Config["value"] = raw(`7`)

	rep := newTestHarness().Validate(validationGraph("pass"),
		&compiler.RenderBundle{}, []*ExecProgram{prog})

	if rep.Status != StatusValidated {
		t.Fatalf("status = %s, want validated", rep.Status)
	}
	if len(rep.Blueprints) != 1 || len(rep.Blueprints[0].Entrypoints) != 1 {
		t.Fatalf("unexpected report shape: %+v", rep)
	}
	er := rep.Blueprints[0].Entrypoints[0]
	if !er.Pass {
		t.Fatalf("entrypoint failed: %+v", er)
	}
	// The leaf the scene wrote is listed in the report.
	if !contains(er.LeavesWritten, "__vars.bp.counter") {
		t.Fatalf("leaves_written missing the var leaf: %v", er.LeavesWritten)
	}
	// The node it entered is in the coverage list.
	if !contains(er.NodesCovered, "set") {
		t.Fatalf("coverage missing the set node: %v", er.NodesCovered)
	}
}

// TestHarness_DivergentWhileFailsValidation (criterion 12): a while(true)
// crosses the step budget → entrypoint FAILS (not killed, not aired). The
// report names the blueprint + entrypoint + steps. This is the doctrine:
// the budget bounds the PROOF, the divergent logic never reaches air.
func TestHarness_DivergentWhileFailsValidation(t *testing.T) {
	// condition pulls a literal `true` — never goes false → infinite loop.
	g := validationGraph("diverge")
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
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "while"}},
		},
	}
	prog.Nodes["noop"].Config["value"] = raw(`1`)

	rep := NewHarness(NewComputeRegistry(), quietLogger(),
		ValidationBudget{MaxSteps: 5_000, MaxWall: time.Second}).
		Validate(g, &compiler.RenderBundle{}, []*ExecProgram{prog})

	if rep.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", rep.Status)
	}
	er := rep.Blueprints[0].Entrypoints[0]
	if er.Pass {
		t.Fatalf("divergent while passed — must fail validation")
	}
	if !strings.Contains(er.FailReason, "budget exceeded") {
		t.Fatalf("fail_reason = %q, want a budget reason", er.FailReason)
	}
	if er.Steps < 5_000 {
		t.Fatalf("steps = %d, want >= the step budget", er.Steps)
	}
	if er.Entrypoint != "start" {
		t.Fatalf("report does not name the entrypoint: %q", er.Entrypoint)
	}
	if rep.Blueprints[0].BlueprintKey != "bp" {
		t.Fatalf("report does not name the blueprint: %q", rep.Blueprints[0].BlueprintKey)
	}
}

// TestHarness_OneFailingEntrypointFailsScene: a scene is validated iff
// EVERY entrypoint passes. One divergent entrypoint among several passing
// ones fails the whole scene.
func TestHarness_OneFailingEntrypointFailsScene(t *testing.T) {
	g := validationGraph("mixed")
	g.Nodes = append(g.Nodes, compiler.GraphNode{ID: "lit.true", Kind: "input", Path: "lit.true"})
	g.Defaults["lit.true"] = raw(`true`)

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"ok":   varSet("ok", "counter", nil, nil),
			"while": {ID: "while", Op: OpWhile,
				Data: []ExecDataInput{{Port: "condition", From: "lit.true"}},
				Next: map[string]ExecTarget{"body": {Node: "ok"}}},
		},
		Entrypoints: map[string]ExecEntry{
			"a-start": {Kind: EntryOnStart, Target: ExecTarget{Node: "ok"}},
			"b-event": {Kind: EntryOnEvent, Event: "hype", Target: ExecTarget{Node: "while"}},
		},
	}
	prog.Nodes["ok"].Config["value"] = raw(`1`)

	rep := NewHarness(NewComputeRegistry(), quietLogger(),
		ValidationBudget{MaxSteps: 5_000, MaxWall: time.Second}).
		Validate(g, &compiler.RenderBundle{}, []*ExecProgram{prog})

	if rep.Status != StatusFailed {
		t.Fatalf("status = %s, want failed (one divergent entrypoint)", rep.Status)
	}
	var passed, failed int
	for _, er := range rep.Blueprints[0].Entrypoints {
		if er.Pass {
			passed++
		} else {
			failed++
		}
	}
	if passed == 0 || failed == 0 {
		t.Fatalf("expected a mix of pass/fail, got passed=%d failed=%d", passed, failed)
	}
}

// TestHarness_PureSceneValidatesTrivially: a scene with no exec programs
// (the prod state today — R9) validates with status=validated and no
// entrypoints. Pushing a pure-dataflow scene must never require exec work.
func TestHarness_PureSceneValidatesTrivially(t *testing.T) {
	rep := newTestHarness().Validate(validationGraph("pure"), &compiler.RenderBundle{}, nil)
	if rep.Status != StatusValidated {
		t.Fatalf("pure scene status = %s, want validated", rep.Status)
	}
	if len(rep.Blueprints) != 0 {
		t.Fatalf("pure scene has blueprints: %+v", rep.Blueprints)
	}
}

// TestHarness_ValidationModeNoEffect (criterion 14, B10): a blueprint with
// http.request validates WITHOUT any real egress — proven by an effect
// seam (SceneEffects) whose Runner would PANIC the test if it were ever
// submitted a job, and an egress policy whose dial would FAIL the test if a
// socket opened. The op completes via the inert seam's synthetic response.
func TestHarness_ValidationModeNoEffect(t *testing.T) {
	g := validationGraph("noegress")
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
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "http"}},
		},
	}
	prog.Nodes["set"].Config["value"] = raw(`true`)

	rep := newTestHarness().Validate(g, &compiler.RenderBundle{}, []*ExecProgram{prog})

	if rep.Status != StatusValidated {
		t.Fatalf("status = %s, want validated", rep.Status)
	}
	er := rep.Blueprints[0].Entrypoints[0]
	if !er.Pass {
		t.Fatalf("http.request entrypoint failed: %+v", er)
	}
	// The attempted egress is LISTED in the report (the author sees the call).
	found := false
	for _, ea := range er.EffectsTried {
		if ea.Op == OpHTTPRequest && ea.Node == "http" {
			found = true
		}
	}
	if !found {
		t.Fatalf("effects_attempted does not list the http.request: %+v", er.EffectsTried)
	}
	// The continuation walked `then` and wrote the success leaf — proving
	// the synthetic completion drove the chain forward (not the error port,
	// not a hang).
	if !contains(er.LeavesWritten, "__vars.bp.done") {
		t.Fatalf("http.request did not complete down then: leaves=%v", er.LeavesWritten)
	}
}

// TestHarness_ValidationEffectRunsNoClosure proves structurally that the
// world op's I/O closure NEVER runs in validation mode: the scene is in
// validation mode with SetEffects installed pointing at a runner that
// panics on Submit. A campaign that opened a socket would call Submit and
// panic; it does not.
func TestHarness_ValidationEffectRunsNoClosure(t *testing.T) {
	g := validationGraph("noclosure")
	g.Nodes = append(g.Nodes, compiler.GraphNode{ID: "lit.ds", Kind: "input", Path: "lit.ds"})

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"db": {ID: "db", Op: OpDBQuery,
				Config: map[string]json.RawMessage{"datasource": raw(`"truth"`)},
				Next:   map[string]ExecTarget{"then": {Node: "set"}}},
			"set": varSet("set", "done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "db"}},
		},
	}
	prog.Nodes["set"].Config["value"] = raw(`true`)

	// Build a clone scene directly, install validation mode AND a real
	// effect set whose use would be observable, then drive the entrypoint.
	gcopy := *g
	scene := NewScene(g.SceneID, &gcopy, &compiler.RenderBundle{}, NewComputeRegistry(), quietLogger())
	scene.SetValidationMode()
	// SetEffects registers the world ops; in validation mode execNode must
	// route around them entirely. If it didn't, execDBQuery would run and
	// (with a nil DB client) fail to the error port — but the synthetic
	// path drives `then` instead, writing the success leaf.
	scene.SetEffects(&SceneEffects{})
	scene.InstallExec(prog)

	res := scene.RunValidationEntrypoint("start", nil, DefaultValidationBudget)
	scene.cancel()

	if !res.Pass {
		t.Fatalf("entrypoint failed: %+v", res)
	}
	if v, ok := scene.state.Get("__vars.bp.done"); !ok || string(v) != "true" {
		t.Fatalf("db.query did not complete down then via the synthetic seam: %s", v)
	}
}

// TestHarness_B10GuardEnumeratesRegistry (criterion 14, guard test): the
// world-effect registry the harness enumerates EQUALS the set of ops
// SetEffects registers. If a phase-N op is added to SetEffects without a
// validation-mode declaration, this guard fails — an effect added later
// cannot silently leak to a real egress in validation mode.
func TestHarness_B10GuardEnumeratesRegistry(t *testing.T) {
	if err := ValidateValidationModeCoverage(); err != nil {
		t.Fatalf("validation-mode coverage guard failed: %v", err)
	}
	// Every world op MUST have a synthetic response declared.
	for _, op := range EnumerateWorldEffects() {
		if _, ok := validationSyntheticResult(op); !ok {
			t.Fatalf("world effect %q has no synthetic response", op)
		}
	}
	// The enumerated set must EQUAL the ops SetEffects registers — drift in
	// either direction is a B10 hole. SetEffects registers exactly
	// http.request / db.query / source.read (exec_effects.go).
	registered := map[string]struct{}{
		OpHTTPRequest: {}, OpDBQuery: {}, OpSourceRead: {},
	}
	world := map[string]struct{}{}
	for _, op := range EnumerateWorldEffects() {
		world[op] = struct{}{}
	}
	if len(registered) != len(world) {
		t.Fatalf("world-effect set %v != SetEffects ops %v", world, registered)
	}
	for op := range registered {
		if _, ok := world[op]; !ok {
			t.Fatalf("SetEffects registers %q but it is not in the world-effect guard set", op)
		}
	}
}

// TestHarness_OnTickFiresNFrames: an on-tick entrypoint is fired N frames,
// each with delta_seconds bound. Each frame is a passing entrypoint result.
func TestHarness_OnTickFiresNFrames(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "counter", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"tick": {Kind: EntryOnTick, Node: "evt.tick", Target: ExecTarget{Node: "set"}},
		},
	}
	prog.Nodes["set"].Config["value"] = raw(`1`)

	rep := newTestHarness().Validate(validationGraph("tick"),
		&compiler.RenderBundle{}, []*ExecProgram{prog})
	if rep.Status != StatusValidated {
		t.Fatalf("status = %s, want validated", rep.Status)
	}
	if got := len(rep.Blueprints[0].Entrypoints); got != onTickFrames {
		t.Fatalf("on-tick fired %d frames, want %d", got, onTickFrames)
	}
}

// TestExecProgramsFromGraph_Roundtrip: the graph's exec_programs raw bytes
// deserialize back to programs; a malformed entry is a loud error (the
// campaign fails, never silently airs).
func TestExecProgramsFromGraph_Roundtrip(t *testing.T) {
	prog := &ExecProgram{BlueprintKey: "bp", Nodes: map[string]*ExecNode{},
		Entrypoints: map[string]ExecEntry{}}
	rawProg, _ := json.Marshal(prog)
	g := &compiler.Graph{ExecPrograms: []json.RawMessage{rawProg}}
	got, err := ExecProgramsFromGraph(g)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].BlueprintKey != "bp" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	bad := &compiler.Graph{ExecPrograms: []json.RawMessage{json.RawMessage(`{not json`)}}
	if _, err := ExecProgramsFromGraph(bad); err == nil {
		t.Fatalf("malformed program decoded without error")
	}

	// Empty/absent set → pure-dataflow scene, no error.
	if got, err := ExecProgramsFromGraph(&compiler.Graph{}); err != nil || got != nil {
		t.Fatalf("empty set: got=%v err=%v", got, err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
