package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/store"
)

// AirValidator is the air-eligibility read seam the validation gate
// consults (ADR 003 §3.2.2 / ADR 016 Amendment 1, issue #247). It is the
// single method the gate needs from a validation source, factored out so
// the embedded-local profile can substitute the transport WITHOUT touching
// the gate logic:
//
//   - antenne        → the store (PG row), byte-for-byte the prior path.
//   - embedded-local → store.MirrorValidator (a validated-record seed file).
//
// The signature is exactly store.Store.IsVersionValidated, so the antenne
// store satisfies it directly via storeAirValidator. Posture is identical
// either way: missing record → (false, nil); read error → (false, err);
// validated → (true, nil).
type AirValidator interface {
	IsVersionValidated(ctx context.Context, sceneID uuid.UUID, sceneVersion, harnessVersion string) (bool, error)
}

// storeAirValidator adapts a store.Store to AirValidator. It is the antenne
// default (and the embedded-local fallback when no mirror is configured),
// preserving the exact prior gate behaviour: the gate reads the DB row.
type storeAirValidator struct{ st store.Store }

// NewStoreAirValidator wraps a store.Store as an AirValidator — the antenne
// air-eligibility source (the PG `validated` row). cmd/orion uses it to wire
// the boot-path validator (ExecForBoot) on antenne; the request gate uses the
// same adapter implicitly when PublicDeps.AirValidator is nil.
func NewStoreAirValidator(st store.Store) AirValidator { return storeAirValidator{st} }

func (a storeAirValidator) IsVersionValidated(ctx context.Context, sceneID uuid.UUID, sceneVersion, harnessVersion string) (bool, error) {
	return a.st.IsVersionValidated(ctx, sceneID, sceneVersion, harnessVersion)
}

// airValidator returns the validator the gate must consult for these deps:
// the explicitly-wired AirValidator (embedded-local mirror) when present,
// else the store (antenne — unchanged). Centralised so every gate path
// resolves the source identically.
func (deps PublicDeps) airValidator() AirValidator {
	if deps.AirValidator != nil {
		return deps.AirValidator
	}
	return storeAirValidator{deps.Store}
}
