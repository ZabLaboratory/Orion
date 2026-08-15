package bluehost

import (
	"errors"
	"testing"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

// These exercise the bound-enforcement mechanism itself (stepUntilSettledOrExhausted,
// runBoundedSteps) through injected closures — no real blueruntime program or
// instance needed. This is deliberate: today's opcode set settles in a
// single Step call whenever nothing external is ever dispatched (Mode
// Execute admits no self-queuing path — see ValidateProgram's doc), so the
// step-budget-exceeded branch is not reachable through a real program via
// the public API today. It is still real, load-bearing code — a future
// opcode, or a caller passing a deliberately tiny maxSteps, can reach it —
// and it is tested here directly rather than left unverified.

// TestStepUntilSettledOrExhausted_StopsWhenIdle proves the normal
// settlement path: TWO consecutive "idle" results end the loop with a nil
// verdict. Exactly two, not one — the FIRST call's Status=="idle" is never
// trusted on its own (Bastion finding: runtime.go's Step hardcodes
// Status="idle" on the pendingStart-consuming call regardless of whether
// its own walk just admitted a synthetic effect completion onto the inbox;
// only the SECOND call's report is freshly computed against the inbox and
// trustworthy — see stepUntilSettledOrExhausted's doc). This is the exact
// mechanism that lets a previewNoop-synthesized effect completion's
// downstream continuation genuinely run instead of being silently skipped
// by trusting an inaccurate first report.
func TestStepUntilSettledOrExhausted_StopsWhenIdle(t *testing.T) {
	calls := 0
	step := func() (blueruntime.StepResult, error) {
		calls++
		return blueruntime.StepResult{Status: "idle"}, nil
	}
	if err := stepUntilSettledOrExhausted(step, 10); err != nil {
		t.Fatalf("expected nil (settled), got %+v", err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 2 calls (the first idle report is never trusted alone) — settling after 1 would silently skip a completion-gated continuation, got %d", calls)
	}
}

// TestStepUntilSettledOrExhausted_RefusesUnconsumedInvocation proves the
// second Bastion-mandated guard: any StepResult.Invocations emitted by a
// call — the async core.effect.invoke@1 protocol's "real" branch, which
// nothing on this path consumes (unlike bluehost.Host.Step) — is refused by
// name, never silently dropped, even on an otherwise-idle result.
func TestStepUntilSettledOrExhausted_RefusesUnconsumedInvocation(t *testing.T) {
	step := func() (blueruntime.StepResult, error) {
		return blueruntime.StepResult{Status: "idle", Invocations: []map[string]any{{"invocation_id": "synthetic"}}}, nil
	}
	err := stepUntilSettledOrExhausted(step, 10)
	if err == nil {
		t.Fatal("expected refusal for an unconsumed invocation, got nil (servable)")
	}
	if err.Code != "VALIDATE_INVOCATION_EMITTED" {
		t.Fatalf("expected code=VALIDATE_INVOCATION_EMITTED, got %+v", err)
	}
}

// TestStepUntilSettledOrExhausted_PropagatesStepError proves a genuine
// execution failure (any *blueruntime.Error a Step call returns) is
// surfaced as the verdict verbatim, not swallowed into a generic refusal.
func TestStepUntilSettledOrExhausted_PropagatesStepError(t *testing.T) {
	native := &blueruntime.Error{SchemaVersion: blueruntime.ErrorSchema, Code: "PROGRAM_BUDGET_INVALID", Stage: "step", Message: "synthetic"}
	step := func() (blueruntime.StepResult, error) { return blueruntime.StepResult{}, native }
	err := stepUntilSettledOrExhausted(step, 10)
	if err == nil || err.Code != "PROGRAM_BUDGET_INVALID" {
		t.Fatalf("expected the native PROGRAM_BUDGET_INVALID error, got %+v", err)
	}
}

// TestStepUntilSettledOrExhausted_ExhaustsStepBudget proves the step-count
// bound itself: a step function that never reports "idle" and never errors
// (the shape a non-terminating cross-Step cascade would take) is refused,
// named and distinct, after exactly maxSteps calls — never a silent "ok".
func TestStepUntilSettledOrExhausted_ExhaustsStepBudget(t *testing.T) {
	calls := 0
	step := func() (blueruntime.StepResult, error) {
		calls++
		return blueruntime.StepResult{Status: "runnable"}, nil
	}
	err := stepUntilSettledOrExhausted(step, 3)
	if err == nil {
		t.Fatal("expected a step-budget refusal, got nil (servable)")
	}
	if err.Code != "VALIDATE_STEP_BUDGET_EXCEEDED" {
		t.Fatalf("expected code=VALIDATE_STEP_BUDGET_EXCEEDED, got %+v", err)
	}
	if calls != 3 {
		t.Fatalf("expected exactly 3 calls (the budget), got %d", calls)
	}
}

// TestRunBoundedSteps_ExceedsWallClock proves the independent wall-clock
// bound: a step function slow enough to blow a tight maxWall is refused
// with a distinct code, even though it would eventually settle on its own
// (Runtime.Step takes no context/deadline of its own — see runBoundedSteps'
// doc for why this is enforced by racing a goroutine against time.After
// rather than a between-calls check). The margin (5ms sleep vs 100
// microsecond budget, 50x) is chosen to make the race deterministic on a
// loaded CI runner, not to shave the tightest possible bound.
func TestRunBoundedSteps_ExceedsWallClock(t *testing.T) {
	stepStarted := make(chan struct{}, 1)
	stopCalled := make(chan struct{}, 1)
	step := func() (blueruntime.StepResult, error) {
		select { // signal "started" at most once — this may be called more than once
		case stepStarted <- struct{}{}:
		default:
		}
		time.Sleep(5 * time.Millisecond)
		return blueruntime.StepResult{Status: "idle"}, nil
	}
	stop := func() error {
		stopCalled <- struct{}{}
		return nil
	}

	err := runBoundedSteps(step, stop, 10, 100*time.Microsecond)
	if err == nil {
		t.Fatal("expected a wall-clock refusal, got nil (servable)")
	}
	if err.Code != "VALIDATE_WALL_BUDGET_EXCEEDED" {
		t.Fatalf("expected code=VALIDATE_WALL_BUDGET_EXCEEDED, got %+v", err)
	}

	// The abandoned goroutine is still exclusively responsible for stop() —
	// drain it so the test does not exit with a dangling goroutine that
	// could race a later test's -race run.
	select {
	case <-stepStarted:
	case <-time.After(time.Second):
		t.Fatal("worker goroutine never started its step call")
	}
	select {
	case <-stopCalled:
	case <-time.After(time.Second):
		t.Fatal("worker goroutine never called stop() after the timeout")
	}
}

// TestRunBoundedSteps_CallsStopOnNormalCompletion proves stop() runs, from
// the same worker goroutine, on the ordinary (non-timeout) settlement path
// too — the single-actor discipline runBoundedSteps' doc requires
// (InstanceHandle "safe to use from one host actor at a time") never
// depends on which path is taken.
func TestRunBoundedSteps_CallsStopOnNormalCompletion(t *testing.T) {
	stopCalls := 0
	step := func() (blueruntime.StepResult, error) { return blueruntime.StepResult{Status: "idle"}, nil }
	stop := func() error { stopCalls++; return nil }

	if err := runBoundedSteps(step, stop, 10, time.Second); err != nil {
		t.Fatalf("expected nil (settled), got %+v", err)
	}
	if stopCalls != 1 {
		t.Fatalf("expected stop() called exactly once, got %d", stopCalls)
	}
}

// TestRunBoundedSteps_StopErrorOnlySurfacesWhenNoOtherVerdict proves the
// precedence rule: a real execution/budget refusal is never masked by a
// subsequent stop() failure, but a stop() failure DOES surface when the
// steps themselves settled cleanly (leaving no other verdict to report).
func TestRunBoundedSteps_StopErrorOnlySurfacesWhenNoOtherVerdict(t *testing.T) {
	stopErr := errors.New("stop: synthetic failure")

	t.Run("execution refusal wins", func(t *testing.T) {
		step := func() (blueruntime.StepResult, error) {
			return blueruntime.StepResult{}, &blueruntime.Error{SchemaVersion: blueruntime.ErrorSchema, Code: "PROGRAM_BUDGET_INVALID", Stage: "step"}
		}
		err := runBoundedSteps(step, func() error { return stopErr }, 10, time.Second)
		if err == nil || err.Code != "PROGRAM_BUDGET_INVALID" {
			t.Fatalf("expected the execution refusal to win, got %+v", err)
		}
	})

	t.Run("stop error surfaces when otherwise servable", func(t *testing.T) {
		step := func() (blueruntime.StepResult, error) { return blueruntime.StepResult{Status: "idle"}, nil }
		err := runBoundedSteps(step, func() error { return stopErr }, 10, time.Second)
		if err == nil {
			t.Fatal("expected the stop() failure to surface, got nil")
		}
		if err.Message != stopErr.Error() {
			t.Fatalf("expected stop()'s error wrapped, got %+v", err)
		}
	})
}
