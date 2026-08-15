package bluehost

import (
	"errors"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

// validateInstanceID is the fixed InstanceHandle id every ephemeral
// contre-validation instance is started with. Safe as a constant: unlike
// Host.slots, the throwaway *blueruntime.Runtime ValidateProgram builds has
// no shared instance registry for two concurrent calls to collide on — each
// call gets its own independent Runtime and InstanceHandle, so a repeated id
// across requests is inert.
const validateInstanceID = "validate-program"

// ValidateProgram reports whether Orion's Engine B can admit program — the
// C2 contre-validation surface (ADR-BLUE-012 R6 §6.3/§4.3): "seul Orion
// connaît son registre de providers", so a program that compiles cleanly can
// still declare a `requires` capability nothing here actually serves. This
// exercises the SAME Runtime.Load -> Runtime.Start admission path a real
// Host.Prepare/Take runs (checkProviders cross-references program["requires"]
// against providers/policy identically — CAPABILITY_UNAVAILABLE etc. are
// raised from Start, never from Load: Load alone has no providers argument
// at all, so it cannot be where this route's whole reason to exist — an
// unserved capability — is caught). This runs on a throwaway
// blueruntime.Runtime this function builds and discards itself — it NEVER
// touches a Host, NEVER reaches SlotPreview/SlotOnAir, and callers must not
// try to route it through one (Bastion veto, SCENE-VALIDATION-GATE-C2-ORION:
// Host.Prepare/Take refuse an already-occupied slot, so a validation sharing
// either one could collide with the antenne's real preview/on-air instance).
//
// Zero side effects, BY TWO INDEPENDENT LAYERS (Bastion veto: belt AND
// suspenders, neither alone should be the only thing standing between a
// submitted program and Orion's own service-token-bearing egress):
//
//  1. Structural: Start alone never executes anything — an InstanceHandle
//     only fires its on-start entrypoints (and any effect) on its FIRST Step
//     (runtime.go's pendingStart flag is consumed exclusively by Step), and
//     this function calls Load/Start/Stop only, never Step/Dispatch/Tick/
//     Call/WritePlatformEvent/Resolve, ever. Proven adversarially — mode
//     Execute, REAL live EffectHandlers pointed at a spy server — by
//     TestValidateProgram_StartAloneNeverDialsEvenWithLiveExecuteHandlers,
//     specifically so this guarantee does not depend on point 2 below.
//  2. Belt-and-suspenders: Mode is Preview (mirrors Host.Prepare(SlotPreview,
//     ...) — modeFor's doc in host.go) AND EffectHandlers is
//     NewEffectHandlers(EffectDeps{}, Preview) — empty deps, not merely a nil
//     map — so that even a future refactor of this function that starts
//     calling Step would still find every one of the 4 opcodes-of-full-right
//     handlers wired to nil Runner/Egress/DB/ServiceCall, synthetic-preview
//     branch or not.
//
// A nil return means program is servable as-is against providers/policy. A
// non-nil *blueruntime.Error carries the fail-closed reason (Code/Stage/
// Message/Target) straight from the portable runtime — the same taxonomy a
// real Prepare/Take would fail with, since the admission path is identical.
func ValidateProgram(program []byte, providers []map[string]any, policy blueruntime.CapabilityPolicy) (verdict *blueruntime.Error) {
	rt := blueruntime.NewRuntime()

	handle, err := rt.Load(program)
	if err != nil {
		return asRuntimeError(err)
	}

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

	// defer, panic included (Bastion veto): Release/Stop must run on every
	// exit path from here, not only the straight-line one. Stop's own error
	// only overrides verdict when the admission itself already succeeded
	// (verdict == nil) — a real CAPABILITY_* refusal from Start always wins,
	// it is the more specific and more useful diagnosis.
	defer func() {
		if stopErr := rt.Stop(instance, "validate: ephemeral instance discarded"); stopErr != nil && verdict == nil {
			verdict = asRuntimeError(stopErr)
		}
	}()
	return nil
}

// asRuntimeError narrows the portable ABI's generic `error` return to its
// documented concrete type. Every Runtime.Load/Start/Stop failure on the
// paths ValidateProgram reaches IS a *blueruntime.Error today
// (program.go's runtimeError/runtimeErrorTarget are the only constructors
// feeding those returns) — the fallback exists so an ABI change widening the
// error type fails closed with an opaque code instead of a nil-pointer
// verdict silently reading as "servable".
func asRuntimeError(err error) *blueruntime.Error {
	var rtErr *blueruntime.Error
	if errors.As(err, &rtErr) {
		return rtErr
	}
	return &blueruntime.Error{SchemaVersion: blueruntime.ErrorSchema, Code: "VALIDATE_INTERNAL", Stage: "validate", Message: err.Error()}
}
