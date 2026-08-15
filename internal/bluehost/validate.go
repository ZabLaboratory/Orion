package bluehost

import (
	"errors"
	"fmt"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

// validateInstanceID / validateAdmissionInstanceID name the two throwaway
// InstanceHandles ValidateProgram starts from the SAME Load'd handle — see
// that function's doc for why two. Fixed constants are safe: unlike
// Host.slots, this package's throwaway *blueruntime.Runtime has no shared
// instance registry for two concurrent calls (or these two passes) to
// collide on — each gets its own independent InstanceHandle, so a repeated
// id across requests, or across the two passes, is inert.
const (
	validateInstanceID          = "validate-program"
	validateAdmissionInstanceID = "validate-program-admission"
)

// defaultValidateMaxSteps / defaultValidateMaxWall mirror
// internal/config.Config's ORION_VALIDATION_MAX_STEPS (1_000_000) /
// ORION_VALIDATION_MAX_WALL_S (5s) defaults — internal/runtime's
// DefaultValidationBudget, the platform's existing answer to "how long may
// a validation run" (Engine A's /validate/simulate harness,
// internal/runtime/validation_harness.go). Reused verbatim per instruction
// rather than inventing a second number.
//
// Unlike runtime.NewHarness's "both zero" fallback convention, maxSteps and
// maxWall each fall back to their own default INDEPENDENTLY (see
// ValidateProgram) — a deliberate divergence, not an oversight: an operator
// zeroing one budget dimension on a route that executes caller-submitted
// code must never silently read as "unlimited" on that dimension.
const (
	defaultValidateMaxSteps uint64        = 1_000_000
	defaultValidateMaxWall  time.Duration = 5 * time.Second
)

// ValidateProgram reports whether Orion's Engine B can actually RUN program
// — the C2 contre-validation surface (ADR-BLUE-012 R6 §6.3/§4.3, scope
// widened per the porteur: "il faut qu'on puisse vraiment tout valider,
// tout — pas de concession"). Admission alone (schema/ABI/capabilities,
// checkProviders at Start) does not catch a node failing on its own inputs,
// or a program-declared budget it blows through on its very first
// dispatch — so, after admission, this drives an instance forward with a
// BOUNDED number of Runtime.Step calls until it settles or fails for real.
//
// TWO Start calls on the SAME Load'd handle (Bastion finding, no single
// mode gives full coverage):
//
//   - Pass A, Mode Execute: admission ONLY (Start, immediately Stop, no
//     Step) — checkProviders requires operation["execute"]=="allowed" under
//     this mode, the stricter, more representative contract a real on-air
//     Take uses (operation["preview"] is a DIFFERENT, potentially looser
//     field Preview checks instead). Closes the "Preview says yes, Execute
//     says no" gap: validating under the weaker mode could pass a
//     capability a real Take would still refuse CAPABILITY_MODE_UNSUPPORTED.
//   - Pass B, Mode Preview: the instance actually Step'd under budget.
//     Deliberately NOT Execute, for a reason as important as Pass A's own:
//     a core.effect.invoke@1 node's invocation is parked in pendingEffects
//     and NEVER completes under Execute (nothing here ever calls
//     Complete()) — the graph stalls at the first effect node, and
//     everything downstream of its completion_topic is never walked. Under
//     Preview, a noop-capable provider (walker.go's previewNoop) synthesizes
//     the completion immediately and admits it, so a completion-gated
//     continuation genuinely runs. Execute closes the ADMISSION delta;
//     Preview closes the EXECUTION delta. Neither alone is "more"; they
//     cover different failure classes.
//
// Both passes exercise checkProviders (CAPABILITY_UNAVAILABLE etc. are
// raised from Start, never Load: Load alone has no providers argument at
// all). Everything runs on a throwaway blueruntime.Runtime this function
// builds and discards itself — it NEVER touches a Host, NEVER reaches
// SlotPreview/SlotOnAir, and callers must not try to route it through one
// (Host.Prepare/Take refuse an already-occupied slot, so a validation
// sharing either one could collide with the antenne's real instance).
//
// EffectHandlers is pinned to NewEffectHandlers(EffectDeps{}, Preview) on
// BOTH passes regardless of each pass's own Mode — StartOptions.Mode and
// NewEffectHandlers' own mode argument are independent (verified against
// the vendored blueruntime source pinned by go.mod, in GOMODCACHE — Mode is
// read in exactly seven non-test places: runtime.go:193,218,280,283,308 and
// walker.go:593,599; none of the seven dispatches to a real provider) — so
// the 4 opcodes-of-full-right always take the synthetic-preview branch on
// both passes.
//
// A SEPARATE, mandatory guard covers the mechanism EffectHandlers does NOT
// reach: core.effect.invoke@1's GENERIC protocol. Under Pass B's Preview
// mode, a provider whose operation is NOT preview:"noop" (Orion's registry
// has one today — core.overlay-app, preview:"emulated") still takes the
// "real invocation" branch and lands an entry in StepResult.Invocations.
// Nothing on this path consumes that — Runtime itself contacts no provider,
// and unlike bluehost.Host.Step (which hands StepResult.Invocations to
// dispatchInvocations, whose real HTTP call runs on a worker pool), this
// function never wires such a consumer. That is an invariant of ABSENCE,
// held by no assertion of its own accord — a future refactor toward
// Host.Step would compose for real, silently, past this function's own
// adversarial spy tests (which only cover the 4 direct opcodes).
// stepUntilSettledOrExhausted asserts StepResult.Invocations is empty after
// every Step and refuses by name (VALIDATE_INVOCATION_EMITTED) rather than
// drop it — the belt that survives that refactor.
//
// maxSteps/maxWall bound Pass B's Step loop — zero values fall back to
// defaultValidateMaxSteps/defaultValidateMaxWall (independently — see that
// constant's doc). Exhausting either, or an emitted invocation, is a named,
// fail-closed refusal, never a silent "ok" and never a 500 — see
// runBoundedSteps for why the wall-clock bound is enforced by racing a
// goroutine against time.After rather than by checking a deadline between
// calls: Runtime.Step takes no context of its own, and a submitted
// program's OWN declared max_steps_per_dispatch/max_execution_ms (which
// blueruntime admits at any positive value, no platform ceiling) bounds a
// SINGLE call's internal work — a between-calls check cannot catch a single
// pathological call, only racing the call itself against a timer can.
//
// A nil return means program is servable as-is against providers/policy — a
// real execution, not merely an admission, ran clean under both passes. A
// non-nil *blueruntime.Error carries the fail-closed reason (Code/Stage/
// Message/Target); for an admission failure this is the same taxonomy a
// real Prepare/Take would fail with, since Pass A's path is identical.
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

	// Pass A — Execute: admission verdict only, nothing kept but the
	// error/nil. Start alone never executes anything (no Step is ever
	// called on this instance), so the immediate Stop is exactly as inert
	// as it was before this function ever ran two passes.
	if verdict := admitUnderExecute(rt, handle, providers, policy); verdict != nil {
		return verdict
	}

	// Pass B — Preview: the instance this function actually steps.
	instance, err := rt.Start(handle, blueruntime.StartOptions{
		InstanceID:     validateInstanceID,
		Mode:           blueruntime.Preview,
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

// admitUnderExecute runs Pass A: Start in Mode Execute against the SAME
// Load'd handle Pass B will also Start, then immediately Stop without ever
// calling Step. Its only output is the admission verdict — checkProviders'
// operation["execute"]=="allowed" contract, the one a real on-air Take
// actually uses.
func admitUnderExecute(rt *blueruntime.Runtime, handle blueruntime.ProgramHandle, providers []map[string]any, policy blueruntime.CapabilityPolicy) *blueruntime.Error {
	instance, err := rt.Start(handle, blueruntime.StartOptions{
		InstanceID:     validateAdmissionInstanceID,
		Mode:           blueruntime.Execute,
		Providers:      providers,
		Policy:         policy,
		EffectHandlers: NewEffectHandlers(EffectDeps{}, blueruntime.Preview),
	})
	if err != nil {
		return asRuntimeError(err)
	}
	if stopErr := rt.Stop(instance, "validate: admission-only pass discarded"); stopErr != nil {
		return asRuntimeError(stopErr)
	}
	return nil
}

// runBoundedSteps races stepping an instance to settlement (via step/stop,
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
		return stepRefusal("VALIDATE_WALL_BUDGET_EXCEEDED", fmt.Sprintf("execution did not settle within the %s validation wall-clock budget", maxWall))
	}
}

// stepUntilSettledOrExhausted calls step repeatedly until it genuinely
// settles, fails, emits an unconsumed effect invocation, or exhausts
// maxSteps.
//
// Settlement is NOT simply "the first StepResult.Status=='idle'": the very
// FIRST call (which consumes InstanceHandle.pendingStart) hardcodes
// Status="idle" in its return regardless of whether its own on-start walk
// just admitted a synthetic effect completion onto the inbox (walker.go's
// previewNoop branch, admitEffectEvent) — runtime.go's Step sets
// instance.status="idle" BEFORE running the walk, and returns that literal,
// not a fresh read. Every SUBSEQUENT call's Status is freshly computed
// AFTER its own walk (checked against len(inbox)) and is trustworthy. This
// route's whole Pass-B rationale is letting a previewNoop-synthesized
// completion carry a downstream continuation forward — trusting the FIRST
// call's report would silently skip exactly that: this refuses to consider
// the instance settled on call index 0 no matter what Status says. Never
// more than one extra call for an on-start with nothing to synthesize.
//
// StepResult.Invocations must be empty on every call — see ValidateProgram's
// doc for why (the async core.effect.invoke@1 protocol's real branch, which
// nothing on this path consumes, unlike bluehost.Host.Step). A non-empty
// Invocations is refused by name (VALIDATE_INVOCATION_EMITTED), never
// silently dropped.
//
// A step call failing outright (a genuine execution failure — a
// program-declared budget breach, a malformed continuation) surfaces with
// its native *blueruntime.Error code, e.g. PROGRAM_BUDGET_INVALID.
// Exhausting maxSteps with neither is VALIDATE_STEP_BUDGET_EXCEEDED.
func stepUntilSettledOrExhausted(step func() (blueruntime.StepResult, error), maxSteps uint64) *blueruntime.Error {
	for i := uint64(0); i < maxSteps; i++ {
		result, err := step()
		if err != nil {
			return asRuntimeError(err)
		}
		if len(result.Invocations) > 0 {
			return stepRefusal("VALIDATE_INVOCATION_EMITTED", fmt.Sprintf("step emitted %d unconsumed effect invocation(s) — refusing rather than dropping them silently", len(result.Invocations)))
		}
		if i > 0 && result.Status == "idle" {
			return nil
		}
	}
	return stepRefusal("VALIDATE_STEP_BUDGET_EXCEEDED", fmt.Sprintf("execution did not settle within %d steps", maxSteps))
}

// stepRefusal builds a named, fail-closed refusal raised by ValidateProgram
// itself (not the portable runtime) during Pass B's step loop — a budget
// exhausted or an unconsumed invocation emitted: never a default "ok",
// never a 500, a distinct code per reason like every other refusal this
// route returns.
func stepRefusal(code, message string) *blueruntime.Error {
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
