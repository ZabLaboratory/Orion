package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
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
// Post-R9-lift (ADR 006 §3.4, issue #106) the same record ALSO keys the
// exec INSTALL on every activation path, through the execForAir seam
// below: push (both branches), validation success, boot reseed, rollback.
//
// Test sessions are NEVER gated — that is where authors iterate freely.
// Authoring is never blocked: a push always persists; only the antenna
// waits for proof.

// sceneNotValidatedCode is the refusal surfaced on every gated path.
const sceneNotValidatedCode = "SCENE_NOT_VALIDATED"

// lsmlGateRejectedCode is the refusal surfaced by the authoring validation
// gate (ADR 002 §3.4 T6 / #I). Unlike SCENE_NOT_VALIDATED — which still
// persists the version and merely holds the antenna — a gate rejection is a
// HARD refusal: the bundle violates a 0-loss / security invariant (hostile
// src, out-of-enum value, dangling/cyclic mask ref, or a blown complexity
// budget), so it is neither persisted nor served. The author must fix the
// source. This is the one place authoring IS blocked, by design.
const lsmlGateRejectedCode = "LSML_GATE_REJECTED"

// isAirEligible reports whether (sceneID, sceneVersion) carries a
// `validated` record for the current harness_version. It FAILS CLOSED: a
// read error returns (false, err) so the caller refuses rather than airing
// an unproven version. A missing record is (false, nil) — refuse, not
// error (the author must run /validate).
//
// The validated record is read through deps.airValidator() (ADR 016
// Amendment 1, #247): the store on antenne (PG row, unchanged), or the
// mirror seed file in embedded-local. The eligibility logic — and this
// fail-closed posture — is identical regardless of the source.
func isAirEligible(ctx context.Context, deps PublicDeps, sceneID uuid.UUID, sceneVersion string) (bool, error) {
	return deps.airValidator().IsVersionValidated(ctx, sceneID, sceneVersion, runtime.HarnessVersion)
}

// execForAir is the SINGLE seam every production activation path uses to
// resolve the exec program set a live scene instance must carry (the R9
// lift, ADR 006 §3.4 / issue #106). It enforces the normative invariant:
//
//	a roster instance carries exec programs IFF its scene_version has a
//	`validated` record for the current harness_version.
//
// It composes the two gates with their distinct postures:
//
//   - isAirEligible (#87) is FAIL-CLOSED: a DB error returns (nil, err)
//     so the caller installs NOTHING and refuses rather than air an
//     unproven version. A missing record is (nil, nil) — not eligible,
//     no programs, no error (the author must run /validate).
//   - ExecProgramsFromGraph is FAIL-LOUD: a corrupt exec artefact of an
//     ALREADY-validated version returns (nil, err) so the lift never
//     silently airs a validated scene with its logic decoded to garbage.
//
// It returns (programs, eligible, err). `eligible` is the antenna-move
// decision the caller needs distinctly from the program set: a VALIDATED
// pure-dataflow scene is eligible yet carries ZERO exec programs, and it
// must still swap onto the antenna. The set is non-empty only for a
// validated exec-bearing version. The single DB read happens here, so a
// call-site never double-queries.
//
// Postures:
//   - err != nil  → fail-closed (DB) or fail-loud (decode); caller
//     refuses and installs NOTHING.
//   - eligible    → install `progs` (possibly empty for pure dataflow).
//   - !eligible   → dataflow only, no exec on air (authoring never blocked).
func execForAir(ctx context.Context, deps PublicDeps, sceneID uuid.UUID, sceneVersion string, graph *compiler.Graph) (progs []*runtime.ExecProgram, eligible bool, err error) {
	eligible, err = isAirEligible(ctx, deps, sceneID, sceneVersion)
	if err != nil {
		return nil, false, err // fail-closed: refuse, install nothing
	}
	if !eligible {
		return nil, false, nil // unproven version: dataflow only, no exec
	}
	progs, err = runtime.ExecProgramsFromGraph(graph)
	if err != nil {
		return nil, false, err // fail-loud: validated-but-corrupt artefact
	}
	return progs, true, nil
}

// ExecForBoot is the boot-path entry to the execForAir seam (ADR 006
// §3.4 path 3, criterion #7). cmd/orion holds no PublicDeps at cold start,
// so this wrapper resolves the validated exec set for one scene over an
// AirValidator directly, sharing the exact same gate composition
// (fail-closed eligibility + fail-loud decode). The validator is the store
// on antenne (unchanged) or the mirror in embedded-local (#247). On ANY
// error it returns nil and logs: a single bad scene loads dataflow-only
// rather than aborting cold start (the boot reseed degrades safe, never
// airing an unproven or unresolved exec set).
func ExecForBoot(ctx context.Context, av AirValidator, sceneID uuid.UUID, sceneVersion string, graph *compiler.Graph, logger *slog.Logger) []*runtime.ExecProgram {
	eligible, err := av.IsVersionValidated(ctx, sceneID, sceneVersion, runtime.HarnessVersion)
	if err != nil {
		logger.Warn("boot reseed: eligibility check failed; loading dataflow-only",
			"scene_id", sceneID.String(), "scene_version", sceneVersion, "err", err)
		return nil
	}
	if !eligible {
		return nil
	}
	progs, err := runtime.ExecProgramsFromGraph(graph)
	if err != nil {
		logger.Error("boot reseed: validated scene's exec artefact is corrupt; loading dataflow-only",
			"scene_id", sceneID.String(), "scene_version", sceneVersion, "err", err)
		return nil
	}
	return progs
}

// loadSceneFromStore fetches a scene's validated pushed-version artefacts and
// loads them into the live roster through the execForAir seam (so a validated
// exec-bearing scene arms its exec). Used by postActiveScene to bring a
// pushed+validated-but-not-loaded scene into the roster before SetActive, so
// activation never fails with `scene not found` (chantier #4). Load is
// idempotent — a no-op swap if the scene is already loaded.
func loadSceneFromStore(ctx context.Context, deps PublicDeps, sceneID uuid.UUID) error {
	pv, err := deps.Store.GetLatestPushedVersion(ctx, sceneID)
	if err != nil {
		return fmt.Errorf("load scene: latest pushed version: %w", err)
	}
	var graph compiler.Graph
	var bundle compiler.RenderBundle
	if err := json.Unmarshal(pv.GraphJSON, &graph); err != nil {
		return fmt.Errorf("load scene: graph json: %w", err)
	}
	if err := json.Unmarshal(pv.BundleJSON, &bundle); err != nil {
		return fmt.Errorf("load scene: bundle json: %w", err)
	}
	progs, _, err := execForAir(ctx, deps, sceneID, pv.SceneVersion, &graph)
	if err != nil {
		return fmt.Errorf("load scene: resolve exec: %w", err)
	}
	deps.Show.LoadExec(sceneID.String(), &graph, &bundle, progs...)
	return nil
}
