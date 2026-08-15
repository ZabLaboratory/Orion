package bluehost

import (
	"errors"
	"fmt"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

// validateInstanceID is the fixed InstanceHandle id every ephemeral
// contre-validation instance is started with. Safe as a constant: unlike
// Host.slots, the throwaway *blueruntime.Runtime ValidateProgram builds has
// no shared instance registry for two concurrent calls to collide on — each
// call gets its own independent Runtime and InstanceHandle, so a repeated id
// across requests is inert.
const validateInstanceID = "validate-program"

// defaultValidateMaxSteps / defaultValidateMaxWall mirror
// internal/config.Config's ORION_VALIDATION_MAX_STEPS (1_000_000) /
// ORION_VALIDATION_MAX_WALL_S (5s) defaults — internal/runtime's
// DefaultValidationBudget, the platform's existing answer to "how long may
// a validation run" (Engine A's /validate/simulate harness,
// internal/runtime/validation_harness.go). Reused verbatim per instruction
// rather than inventing a second number: ValidateProgram's own zero-value
// fallback mirrors runtime.NewHarness's identical "a zero budget falls back
// to the ADR default" posture.
const (
	defaultValidateMaxSteps uint64        = 1_000_000
	defaultValidateMaxWall  time.Duration = 5 * time.Second
)

// ValidateProgram reports whether Orion's Engine B can actually RUN program
// — the C2 contre-validation surface (ADR-BLUE-012 R6 §6.3/§4.3, scope
// widened per the porteur: "il faut qu'on puisse vraiment tout valider,
// tout — pas de concession"). Admission alone (schema/ABI/capabilities,
// checkProviders at Start) does not catch a node failing on its own inputs,
// a program-declared budget it blows through on its very first dispatch, or
// any other execution-time-only failure — so, after a successful Start,
// this drives the instance forward with a BOUNDED number of Runtime.Step
// calls until it settles (StepResult.Status=="idle": inbox drained, nothing
// left admitted — the portable ABI's own "done" signal) or fails for real.
//
// This exercises the SAME Runtime.Load -> Runtime.Start admission path a
// real Host.Prepare/Take runs (checkProviders cross-references
// program["requires"] against providers/policy identically —
// CAPABILITY_UNAVAILABLE etc. are raised from Start, never Load: Load alone
// has no providers argument at all). It runs on a throwaway
// blueruntime.Runtime this function builds and discards itself — it NEVER
// touches a Host, NEVER reaches SlotPreview/SlotOnAir, and callers must not
// try to route it through one (Bastion veto: Host.Prepare/Take refuse an
// already-occupied slot, so a validation sharing either one could collide
// with the antenne's real preview/on-air instance).
//
// Mode is Execute, not Preview — deliberately, to close the "Preview says
// yes, Execute says no" gap: checkProviders only requires
// operation["execute"]=="allowed" when Mode is Execute (operation["preview"]
// is the field it checks under Preview, a DIFFERENT, potentially looser
// contract per capability). Validating under the weaker mode would let a
// preview-only capability pass here and still fail CAPABILITY_MODE_UNSUPPORTED
// on a real on-air Take — a latent lie this route exists specifically not to
// tell. This is safe: StartOptions.Mode and NewEffectHandlers' own mode
// argument are INDEPENDENT (verified against the vendored blueruntime
// source — Mode is read in exactly four places: two Start-time argument
// checks, checkProviders' preview/execute field branch, and walker.go's
// core.effect.invoke@1 previewNoop/real-invocation-record branch, which
// never dispatches to a provider either way — StepResult.Invocations is
// inert data this function never reads). EffectHandlers below is pinned to
// NewEffectHandlers(EffectDeps{}, Preview) regardless of StartOptions.Mode,
// so the 4 opcodes-of-full-right ALWAYS take the synthetic-preview branch —
// belt-and-suspenders proven, now including a real Step, by
// TestValidateProgram_NeverDialsEvenThoughStepReallyExecutes.
//
// Zero side effects even though Step genuinely runs the graph: no real
// network/DB/service call the graph's own nodes could trigger, because the
// only channel to real I/O (EffectHandlers) is hardcoded empty/synthetic —
// see that test and TestValidateProgram_StartAloneNeverDialsEvenWithLiveExecuteHandlers
// (Start alone, the narrower foundational fact) for the adversarial proof.
//
// maxSteps/maxWall bound the Step loop — zero values fall back to
// defaultValidateMaxSteps/defaultValidateMaxWall. Exhausting either is a
// named, fail-closed refusal (VALIDATE_STEP_BUDGET_EXCEEDED /
// VALIDATE_WALL_BUDGET_EXCEEDED), never a silent "ok" and never a 500 — see
// runBoundedSteps for why the wall-clock bound is enforced by racing a
// goroutine against time.After rather than by checking a deadline between
// calls: Runtime.Step takes no context of its own, and a submitted
// program's OWN declared max_steps_per_dispatch/max_execution_ms (which
// blueruntime admits at any positive value, no platform ceiling) bounds a
// SINGLE call's internal work — a between-calls check cannot catch a single
// pathological call, only racing the call itself against a timer can.
//
// A nil return means program is servable as-is against providers/policy — a
// real execution, not merely an admission, ran clean. A non-nil
// *blueruntime.Error carries the fail-closed reason (Code/Stage/Message/
// Target); for an admission failure this is the same taxonomy a real
// Prepare/Take would fail with, since that path is identical.
func ValidateProgram(program []byte, providers []map[string]any, policy blueruntime.CapabilityPolicy, maxSteps uint64, maxWall time.Duration) *blueruntime.Error {
	if maxSteps == 0 {
		maxSteps = defaultValidateMaxSteps
	}
	if maxWall <= 0 {
		maxWall = defaultValidateMaxWall
	}

	rt := blueruntime.NewRuntime()

	handle, err := rt.Load(program)
	if err != nil {
		return asRuntimeError(err)
	}

	instance, err := rt.Start(handle, blueruntime.StartOptions{
		InstanceID:     validateInstanceID,
		Mode:           blueruntime.Execute,
		Providers:      providers,
		Policy:         policy,
		EffectHandlers: NewEffectHandlers(EffectDeps{}, blueruntime.Preview),
	})
	if err != nil {
		return asRuntimeError(err)
	}

	return runBoundedSteps(
		func() (blueruntime.StepResult, error) { return rt.Step(instance) },
		func() error { return rt.Stop(instance, "validate: ephemeral instance discarded") },
		maxSteps, maxWall,
	)
}

// runBoundedSteps races stepping instance to settlement (via step/stop,
// closures so this function and its tests never need a real blueruntime
// instance) against a maxWall timer. It runs the step/stop sequence on its
// OWN goroutine and never touches either closure again after handing off —
// InstanceHandle is documented "safe to use from one host actor at a time"
// (host.go), so once the worker goroutine is spawned it is that one actor
// for the rest of this call's lifetime, timeout or not.
//
// On a timeout, the worker goroutine is NOT cancelled — Go has no forced-
// kill for a running goroutine, and Runtime.Step takes no context — it is
// simply abandoned mid-flight and left to call stop() and exit on its own
// whenever the pathological step (or the program's own oversized declared
// budget) eventually returns. The HTTP caller never waits for that: it gets
// the named refusal the instant the deadline passes. The abandoned
// goroutine still never fires a real effect (same EffectHandlers), still
// never touches a Host, and drops its last reference the moment it exits.
func runBoundedSteps(step func() (blueruntime.StepResult, error), stop func() error, maxSteps uint64, maxWall time.Duration) *blueruntime.Error {
	done := make(chan *blueruntime.Error, 1)

	go func() {
		verdict := stepUntilSettledOrExhausted(step, maxSteps)
		if stopErr := stop(); stopErr != nil && verdict == nil {
			verdict = asRuntimeError(stopErr)
		}
		done <- verdict
	}()

	select {
	case verdict := <-done:
		return verdict
	case <-time.After(maxWall):
		return budgetExceeded("VALIDATE_WALL_BUDGET_EXCEEDED", fmt.Sprintf("execution did not settle within the %s validation wall-clock budget", maxWall))
	}
}

// stepUntilSettledOrExhausted calls step repeatedly until StepResult.Status
// is "idle" (runtime.go: the instance's inbox is drained and nothing new
// was admitted — the portable ABI's own "nothing left to do" signal), step
// itself fails (a genuine execution failure — a program-declared budget
// breach, a malformed continuation — surfaced with its native
// *blueruntime.Error code, e.g. PROGRAM_BUDGET_INVALID), or maxSteps calls
// have run with neither (VALIDATE_STEP_BUDGET_EXCEEDED).
func stepUntilSettledOrExhausted(step func() (blueruntime.StepResult, error), maxSteps uint64) *blueruntime.Error {
	for i := uint64(0); i < maxSteps; i++ {
		result, err := step()
		if err != nil {
			return asRuntimeError(err)
		}
		if result.Status == "idle" {
			return nil
		}
	}
	return budgetExceeded("VALIDATE_STEP_BUDGET_EXCEEDED", fmt.Sprintf("execution did not settle within %d steps", maxSteps))
}

// budgetExceeded builds the named, fail-closed refusal for a program that
// admitted cleanly but never settled within budget — "budget épuisé, non
// validable": never a default "ok", never a 500, a distinct code per
// dimension (step count vs wall clock) like every other refusal this route
// returns.
func budgetExceeded(code, message string) *blueruntime.Error {
	return &blueruntime.Error{SchemaVersion: blueruntime.ErrorSchema, Code: code, Stage: "step", Message: message}
}

// asRuntimeError narrows the portable ABI's generic `error` return to its
// documented concrete type. Every Runtime.Load/Start/Step/Stop failure on
// the paths ValidateProgram reaches IS a *blueruntime.Error today
// (program.go's runtimeError/runtimeErrorTarget and walker.go's own
// runtimeError calls are the only constructors feeding those returns) — the
// fallback exists so an ABI change widening the error type fails closed
// with an opaque code instead of a nil-pointer verdict silently reading as
// "servable".
func asRuntimeError(err error) *blueruntime.Error {
	var rtErr *blueruntime.Error
	if errors.As(err, &rtErr) {
		return rtErr
	}
	return &blueruntime.Error{SchemaVersion: blueruntime.ErrorSchema, Code: "VALIDATE_INTERNAL", Stage: "validate", Message: err.Error()}
}
