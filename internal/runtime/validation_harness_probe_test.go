package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Probe tests for the scene-validation harness and its B10 structural
// inertia (ADR 003 §3.2, issue #87). Refs #87.
//
// Gaps sounded:
//   - wall-budget exceeded path (distinct from step-budget)
//   - ValidationBudgetFrom env-tunable (zero values fall back to default)
//   - MaxSteps=0 disables the step check (wall only)
//   - source.read completes down `then` in validation mode
//   - B10 guard: undeclared world effect → harness returns error
//   - SeedValidationLeaf is a no-op outside validation mode (no live mutation)
//   - Multiple blueprints: one fails → overall StatusFailed
//   - stepBudgetExceeded with MaxSteps=0 disabled
//   - exec-program decode error string format

// --------------------------------------------------------------------------
// 1. Wall budget exceeded → EntrypointResult.FailReason names wall, not steps
// --------------------------------------------------------------------------

// TestHarness_WallBudgetExceededFails (criterion 12, wall dimension):
// a divergent while with a very short wall budget crosses the wall limit
// and the report names "wall budget exceeded" — the wall dimension is
// tested independently of the step budget.
func TestHarness_WallBudgetExceededFails(t *testing.T) {
	g := validationGraph("wallbug")
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

	// Disable step limit, set a very short wall — the while loop drives
	// real CPU so the wall trips first.
	rep := NewHarness(NewComputeRegistry(), quietLogger(),
		ValidationBudget{MaxSteps: 0, MaxWall: 1 * time.Millisecond}).
		Validate(g, &compiler.RenderBundle{}, []*ExecProgram{prog})

	if rep.Status != StatusFailed {
		t.Fatalf("status = %s, want failed (wall budget)", rep.Status)
	}
	er := rep.Blueprints[0].Entrypoints[0]
	if er.Pass {
		t.Fatal("divergent while passed — must fail validation")
	}
	if !strings.Contains(er.FailReason, "wall budget exceeded") {
		t.Fatalf("fail_reason = %q, want wall budget message", er.FailReason)
	}
	// The report must name the blueprint and entrypoint for debuggability.
	if rep.Blueprints[0].BlueprintKey != "bp" {
		t.Fatalf("blueprint key not reported: %q", rep.Blueprints[0].BlueprintKey)
	}
	if er.Entrypoint != "start" {
		t.Fatalf("entrypoint not reported: %q", er.Entrypoint)
	}
}

// --------------------------------------------------------------------------
// 2. ValidationBudgetFrom / NewHarness zero-fallback
// --------------------------------------------------------------------------

// TestValidationBudgetFrom_ZeroFallsBackToDefault: both dimensions at zero
// fall back to the ADR defaults — env-tunable budget never silently
// allows a zero (unbounded) proof.
func TestValidationBudgetFrom_ZeroFallsBackToDefault(t *testing.T) {
	b := ValidationBudgetFrom(0, 0)
	if b.MaxSteps != DefaultValidationBudget.MaxSteps {
		t.Fatalf("MaxSteps = %d, want default %d", b.MaxSteps, DefaultValidationBudget.MaxSteps)
	}
	if b.MaxWall != DefaultValidationBudget.MaxWall {
		t.Fatalf("MaxWall = %v, want default %v", b.MaxWall, DefaultValidationBudget.MaxWall)
	}
}

// TestValidationBudgetFrom_PositiveOverridesDefault: positive values
// override each dimension independently.
func TestValidationBudgetFrom_PositiveOverridesDefault(t *testing.T) {
	b := ValidationBudgetFrom(42, 7*time.Second)
	if b.MaxSteps != 42 {
		t.Fatalf("MaxSteps = %d, want 42", b.MaxSteps)
	}
	if b.MaxWall != 7*time.Second {
		t.Fatalf("MaxWall = %v, want 7s", b.MaxWall)
	}
}

// TestNewHarness_ZeroBudgetFallsBackToDefault: NewHarness with a zero
// budget falls back to the ADR default (same invariant, harness path).
func TestNewHarness_ZeroBudgetFallsBackToDefault(t *testing.T) {
	h := NewHarness(NewComputeRegistry(), quietLogger(), ValidationBudget{})
	if h.budget.MaxSteps != DefaultValidationBudget.MaxSteps {
		t.Fatalf("harness budget steps = %d, want default", h.budget.MaxSteps)
	}
}

// --------------------------------------------------------------------------
// 3. MaxSteps=0 disables step check (wall is the only bound)
// --------------------------------------------------------------------------

// TestStepBudgetExceeded_ZeroDisablesStepCheck: stepBudgetExceeded with
// MaxSteps=0 never fires regardless of how many steps have run — the step
// dimension is explicitly disabled when set to zero.
func TestStepBudgetExceeded_ZeroDisablesStepCheck(t *testing.T) {
	if stepBudgetExceeded(1_000_000_000, 0) {
		t.Fatal("stepBudgetExceeded returned true with MaxSteps=0 (step check must be disabled)")
	}
	if stepBudgetExceeded(0, 0) {
		t.Fatal("stepBudgetExceeded returned true at step 0 with MaxSteps=0")
	}
}

// TestStepBudgetExceeded_PositiveBudgetFires: with a positive MaxSteps the
// check fires at exactly the budget.
func TestStepBudgetExceeded_PositiveBudgetFires(t *testing.T) {
	if stepBudgetExceeded(999, 1000) {
		t.Fatal("budget not exceeded at 999 of 1000")
	}
	if !stepBudgetExceeded(1000, 1000) {
		t.Fatal("budget exceeded at 1000 of 1000 must be true")
	}
	if !stepBudgetExceeded(1001, 1000) {
		t.Fatal("budget exceeded at 1001 of 1000 must be true")
	}
}

// --------------------------------------------------------------------------
// 4. source.read completes down `then` in validation mode (B10 completeness)
// --------------------------------------------------------------------------

// TestHarness_SourceReadValidationModeCompletesThen: a `source.read` op
// in validation mode synthesises a null value and walks `then` — proving
// the third world-touching op's inert path, not only http.request + db.query.
func TestHarness_SourceReadValidationModeCompletesThen(t *testing.T) {
	g := validationGraph("srcread")
	g.Bindings = []compiler.ExternalAdapter{{Key: "live-src", URL: "https://MUST-NOT-CONNECT.invalid"}}

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"src": {ID: "src", Op: OpSourceRead,
				Config: map[string]json.RawMessage{"source_id": raw(`"live-src"`)},
				Next:   map[string]ExecTarget{"then": {Node: "set"}, "error": {Node: "err"}}},
			"set": varSet("set", "got-value", nil, nil),
			"err": varSet("err", "got-error", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "src"}},
		},
	}
	prog.Nodes["set"].Config["value"] = raw(`true`)
	prog.Nodes["err"].Config["value"] = raw(`true`)

	rep := newTestHarness().Validate(g, &compiler.RenderBundle{}, []*ExecProgram{prog})

	if rep.Status != StatusValidated {
		t.Fatalf("source.read validation status = %s, want validated", rep.Status)
	}
	er := rep.Blueprints[0].Entrypoints[0]
	if !er.Pass {
		t.Fatalf("source.read entrypoint failed: %+v", er)
	}
	// Must have walked `then`, not `error`.
	if !contains(er.LeavesWritten, "__vars.bp.got-value") {
		t.Fatalf("source.read did not complete down then: leaves=%v", er.LeavesWritten)
	}
	if contains(er.LeavesWritten, "__vars.bp.got-error") {
		t.Fatalf("source.read completed down error port (must use then in validation mode): leaves=%v", er.LeavesWritten)
	}
	// The attempt is listed in the report (author sees the call).
	found := false
	for _, ea := range er.EffectsTried {
		if ea.Op == OpSourceRead && ea.Node == "src" {
			found = true
		}
	}
	if !found {
		t.Fatalf("effects_attempted does not list source.read: %v", er.EffectsTried)
	}
}

// --------------------------------------------------------------------------
// 5. B10 guard: ValidateValidationModeCoverage fails on undeclared op
// --------------------------------------------------------------------------

// TestB10Guard_UndeclaredWorldEffectFails: a world effect added to the
// SINGLE registration table (worldEffectRegistrations — the same table
// SetEffects installs from) WITHOUT a corresponding synthetic response
// makes ValidateValidationModeCoverage fail. The guard reflects the live
// registry SetEffects builds, so this proves the introspective invariant:
// a new op reachable through SetEffects but missing validation-mode
// inertia cannot pass the guard — it would leak to a real egress in
// validation mode otherwise.
//
// The test appends to the table, then restores it — it is single-goroutine
// and the slice is only mutated here, so this is race-safe in the test
// runner.
func TestB10Guard_UndeclaredWorldEffectFails(t *testing.T) {
	const ghost = "__probe.ghost.op"
	saved := worldEffectRegistrations
	// Append an op whose executor is a no-op: it is registered by
	// SetEffects (so the introspective guard sees it) but has NO
	// validationSyntheticResult declared.
	worldEffectRegistrations = append(append([]struct {
		op string
		fn execOpFn
	}{}, worldEffectRegistrations...), struct {
		op string
		fn execOpFn
	}{ghost, func(*Scene, *execTask, *ExecNode, string) execOpOutcome {
		return execOpOutcome{halt: true}
	}})
	defer func() { worldEffectRegistrations = saved }()

	err := ValidateValidationModeCoverage()
	if err == nil {
		t.Fatal("guard returned nil for an undeclared world effect — B10 hole: new op reachable through SetEffects would leak in validation mode")
	}
	if !strings.Contains(err.Error(), ghost) {
		t.Fatalf("error does not name the undeclared op %q: %v", ghost, err)
	}
}

// TestB10Guard_UndeclaredSyntheticResultReturnsFalse: validationSyntheticResult
// returns (nil, false) for an op not in the switch — the harness's fail-closed
// recovery path is exercised by the guard; this unit test proves the invariant
// directly.
func TestB10Guard_UndeclaredSyntheticResultReturnsFalse(t *testing.T) {
	v, ok := validationSyntheticResult("__probe.no-such-op")
	if ok || v != nil {
		t.Fatalf("undeclared op returned ok=%v v=%s — must return (nil, false)", ok, v)
	}
}

// --------------------------------------------------------------------------
// 6. SeedValidationLeaf is a no-op outside validation mode
// --------------------------------------------------------------------------

// TestSeedValidationLeaf_NoopOutsideValidationMode: calling SeedValidationLeaf
// on a live (non-validation) scene must not write any state — the guard
// prevents inadvertent mutation of a running scene.
func TestSeedValidationLeaf_NoopOutsideValidationMode(t *testing.T) {
	g := varsGraph("live")
	scene := NewScene("live", g, &compiler.RenderBundle{}, NewComputeRegistry(), quietLogger())
	// Do NOT call SetValidationMode.

	scene.SeedValidationLeaf("__vars.bp.should-not-appear", raw(`"injected"`))

	if _, ok := scene.state.Get("__vars.bp.should-not-appear"); ok {
		t.Fatal("SeedValidationLeaf wrote state on a non-validation scene (live mutation guard broken)")
	}
}

// --------------------------------------------------------------------------
// 7. Multiple blueprints: one fails → overall StatusFailed
// --------------------------------------------------------------------------

// TestHarness_MultipleBlueprintsOneFailsScene: two blueprints —
// one terminates fine, one diverges. The scene-level status must be
// StatusFailed; the passing blueprint's results remain Pass=true.
func TestHarness_MultipleBlueprintsOneFailsScene(t *testing.T) {
	g := validationGraph("multi-bp")
	g.Nodes = append(g.Nodes, compiler.GraphNode{ID: "lit.true", Kind: "input", Path: "lit.true"})
	g.Defaults["lit.true"] = raw(`true`)

	goodProg := &ExecProgram{
		BlueprintKey: "alpha",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "counter", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "set"}},
		},
	}
	goodProg.Nodes["set"].Config["value"] = raw(`1`)

	badProg := &ExecProgram{
		BlueprintKey: "beta",
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
	badProg.Nodes["noop"].Config["value"] = raw(`1`)

	rep := NewHarness(NewComputeRegistry(), quietLogger(),
		ValidationBudget{MaxSteps: 2_000, MaxWall: time.Second}).
		Validate(g, &compiler.RenderBundle{}, []*ExecProgram{goodProg, badProg})

	if rep.Status != StatusFailed {
		t.Fatalf("status = %s, want failed (one divergent blueprint)", rep.Status)
	}
	// The passing blueprint is still reported Pass=true.
	var alphaPass, betaFail bool
	for _, br := range rep.Blueprints {
		for _, er := range br.Entrypoints {
			switch br.BlueprintKey {
			case "alpha":
				if er.Pass {
					alphaPass = true
				}
			case "beta":
				if !er.Pass {
					betaFail = true
				}
			}
		}
	}
	if !alphaPass {
		t.Fatal("alpha (terminating) blueprint did not pass")
	}
	if !betaFail {
		t.Fatal("beta (divergent) blueprint did not fail")
	}
}

// --------------------------------------------------------------------------
// 8. execProgramDecodeError error message includes index
// --------------------------------------------------------------------------

// TestExecProgramsFromGraph_DecodeErrorNamesIndex: a malformed exec
// program in position N produces an error that names the index, so the
// author can find which blueprint entry is broken.
func TestExecProgramsFromGraph_DecodeErrorNamesIndex(t *testing.T) {
	g := &compiler.Graph{
		ExecPrograms: []json.RawMessage{
			json.RawMessage(`{"blueprint_key":"ok","nodes":{},"entrypoints":{}}`),
			json.RawMessage(`{bad json`),
		},
	}
	_, err := ExecProgramsFromGraph(g)
	if err == nil {
		t.Fatal("malformed entry at index 1 decoded without error")
	}
	if !strings.Contains(err.Error(), "1") {
		t.Fatalf("decode error does not name the index: %v", err)
	}
}

// --------------------------------------------------------------------------
// 9. Harness report carries budget dimensions
// --------------------------------------------------------------------------

// TestHarness_ReportCarriesBudgetDimensions: the report's budget section
// reflects the harness configuration — env-tunable budgets are observable.
func TestHarness_ReportCarriesBudgetDimensions(t *testing.T) {
	budget := ValidationBudget{MaxSteps: 77_777, MaxWall: 3 * time.Second}
	h := NewHarness(NewComputeRegistry(), quietLogger(), budget)
	rep := h.Validate(validationGraph("budgetcheck"), &compiler.RenderBundle{}, nil)

	if rep.Budget.MaxSteps != 77_777 {
		t.Fatalf("report budget MaxSteps = %d, want 77777", rep.Budget.MaxSteps)
	}
	if rep.Budget.MaxWallMS != 3000 {
		t.Fatalf("report budget MaxWallMS = %d, want 3000", rep.Budget.MaxWallMS)
	}
}

// --------------------------------------------------------------------------
// 10. RunValidationEntrypoint: unknown entrypoint → fail, no panic
// --------------------------------------------------------------------------

// TestRunValidationEntrypoint_UnknownEntrypoint: firing an entrypoint
// that does not exist in the exec program fails gracefully without a
// panic. The harness must fail the scene loudly, not silently pass.
func TestRunValidationEntrypoint_UnknownEntrypoint(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes:        map[string]*ExecNode{},
		Entrypoints:  map[string]ExecEntry{},
	}
	g := validationGraph("unknown-ep")
	scene := NewScene("unknown-ep", g, &compiler.RenderBundle{}, NewComputeRegistry(), quietLogger())
	scene.SetValidationMode()
	scene.InstallExec(prog)

	res := scene.RunValidationEntrypoint("nonexistent", nil, DefaultValidationBudget)
	scene.cancel()

	if res.Pass {
		t.Fatal("unknown entrypoint returned Pass=true")
	}
	if !strings.Contains(res.FailReason, "unknown entrypoint") {
		t.Fatalf("fail_reason = %q, want 'unknown entrypoint'", res.FailReason)
	}
}

// --------------------------------------------------------------------------
// 11. RunValidationEntrypoint with no exec program → fail, no panic
// --------------------------------------------------------------------------

// TestRunValidationEntrypoint_NoExecProgram: firing an entrypoint on a
// scene with no installed exec program fails gracefully.
func TestRunValidationEntrypoint_NoExecProgram(t *testing.T) {
	g := validationGraph("no-prog")
	scene := NewScene("no-prog", g, &compiler.RenderBundle{}, NewComputeRegistry(), quietLogger())
	scene.SetValidationMode()
	// No InstallExec.

	res := scene.RunValidationEntrypoint("start", nil, DefaultValidationBudget)
	scene.cancel()

	if res.Pass {
		t.Fatal("scene with no exec program returned Pass=true")
	}
	if res.FailReason == "" {
		t.Fatal("fail_reason is empty for scene with no exec program")
	}
}
