package providers

import (
	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

// Policy builds the host-side CapabilityPolicy admission gate (ADR-BLUE-012
// §6.6: "policy preview/on-air"). checkProviders only ever calls it with
// (requirement, provider, mode) — never the per-call request payload — so
// this is the coarsest decision this hook can make: whether the capability
// is allowed to admit AT ALL for this deployment. The fine-grained
// per-request check (e.g. the actual URL host for core.http.request) is
// enforced by the effect executor at dispatch time, against
// internal/effects.EgressPolicy — not here.
//
// httpEgressAllowed is the deployment's coarse posture for core.http.request
// (true iff internal/effects.EgressPolicy has a non-empty host allowlist —
// an empty allowlist is the platform's existing deny-all default, see
// internal/effects/egress.go). Every other capability admits unconditionally
// at this layer; wire a new deployment-level gate here if one is added.
func Policy(httpEgressAllowed bool) blueruntime.CapabilityPolicy {
	return func(requirement, _ map[string]any, _ blueruntime.Mode) bool {
		if requirement["capability"] == "core.http.request" {
			return httpEgressAllowed
		}
		return true
	}
}
