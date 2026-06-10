package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// The scene-validation enforcement gate (ADR 003 §3.2.2, issue #87). A
// version is air-eligible iff it carries a `validated` record for the
// CURRENT harness_version. The gate covers EVERY path that activates or
// mutates the live graph (B3 + B-rollback, both critical):
//
//   - POST /show/active-scene        (postActiveScene, show.go)
//   - push-swap of an active scene   (pushScene, scenes_push.go)
//   - rollback                       (handleRollback, scenes_push.go)
//
// Test sessions are NEVER gated — that is where authors iterate freely.
// Authoring is never blocked: a push always persists; only the antenna
// waits for proof.

// sceneNotValidatedCode is the refusal surfaced on every gated path.
const sceneNotValidatedCode = "SCENE_NOT_VALIDATED"

// isAirEligible reports whether (sceneID, sceneVersion) carries a
// `validated` record for the current harness_version. It FAILS CLOSED: a
// DB error returns (false, err) so the caller refuses rather than airing
// an unproven version. A missing record is (false, nil) — refuse, not
// error (the author must run /validate).
func isAirEligible(ctx context.Context, deps PublicDeps, sceneID uuid.UUID, sceneVersion string) (bool, error) {
	return deps.Store.IsVersionValidated(ctx, sceneID, sceneVersion, runtime.HarnessVersion)
}
