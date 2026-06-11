package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// The scene-validation gate endpoints (ADR 003 §3.2.2, issue #87):
//
//   POST /api/v1/scenes/{id}/validate     — run the campaign (async) on the
//                                            latest pushed version.
//   GET  /api/v1/scenes/{id}/validation?v= — status + report.
//
// Enforcement of SCENE_NOT_VALIDATED lives on the activation/mutation
// paths (postActiveScene, pushScene push-swap, handleRollback) — see
// gate.go. The gate is the capstone that lets a VALIDATED exec-bearing
// scene reach air. Post-R9-lift (ADR 006 §3.4, issue #106) this file is
// ALSO one of the activation paths: on validation SUCCESS, runCampaign
// re-loads the roster instance through execForAir (reloadAfterValidation)
// — both arming an off-air scene's exec ahead of activation AND executing
// the deferred swap of an active scene (ADR 003 criterion #15). The
// install stays keyed on the validation record the campaign just wrote;
// no path bypasses the #87 gate.

// validationRunner serialises campaigns per scene so two concurrent
// POST /validate on the same scene don't both run the (CPU-bound)
// campaign. It is process-local — a single Orion owns its show.
type validationRunner struct {
	mu      sync.Mutex
	running map[string]struct{}
}

func newValidationRunner() *validationRunner {
	return &validationRunner{running: map[string]struct{}{}}
}

func (r *validationRunner) tryStart(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.running[key]; ok {
		return false
	}
	r.running[key] = struct{}{}
	return true
}

func (r *validationRunner) done(key string) {
	r.mu.Lock()
	delete(r.running, key)
	r.mu.Unlock()
}

// postValidate handles POST /api/v1/scenes/{id}/validate. It launches the
// campaign asynchronously against the scene's latest pushed version and
// returns 202 with the (scene_id, scene_version, harness_version) the
// campaign targets. The result is persisted on completion; the caller
// polls GET /validation?v= for the verdict.
func postValidate(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		sceneID, ok := parseUUID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene id"})
			return
		}
		pv, err := deps.Store.GetLatestPushedVersion(r.Context(), sceneID)
		if err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}

		key := sceneID.String() + "|" + pv.SceneVersion
		if !deps.ValidationRunner.tryStart(key) {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "VALIDATION_IN_PROGRESS"})
			return
		}

		graph := &compiler.Graph{}
		if err := json.Unmarshal(pv.GraphJSON, graph); err != nil {
			deps.ValidationRunner.done(key)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		bundle := &compiler.RenderBundle{}
		_ = json.Unmarshal(pv.BundleJSON, bundle)

		// Run detached: a campaign is CPU-bound and off the live path
		// (ADR §3.2.2). Use a fresh background context so a client
		// disconnect does not abort the proof.
		go runCampaign(deps, sceneID, pv.SceneVersion, graph, bundle, key)

		writeJSON(w, http.StatusAccepted, map[string]string{
			"scene_id":        sceneID.String(),
			"scene_version":   pv.SceneVersion,
			"harness_version": runtime.HarnessVersion,
			"status":          "running",
		})
	})
}

// runCampaign executes one validation campaign and persists the record.
// Runs on its own goroutine. The B10 structural guard runs FIRST: if any
// registered world-touching effect lacks a declared validation-mode
// behaviour, the campaign fails the scene rather than risk a real egress.
func runCampaign(deps PublicDeps, sceneID uuid.UUID, sceneVersion string, graph *compiler.Graph, bundle *compiler.RenderBundle, key string) {
	defer deps.ValidationRunner.done(key)
	ctx, cancel := context.WithTimeout(context.Background(), deps.Config.ValidationTimeout)
	defer cancel()

	report, status := computeCampaign(deps, graph, bundle)
	raw, err := json.Marshal(report)
	if err != nil {
		raw = json.RawMessage(`{}`)
	}
	rec := store.SceneValidation{
		SceneID:        sceneID,
		SceneVersion:   sceneVersion,
		HarnessVersion: runtime.HarnessVersion,
		Status:         string(status),
		Report:         raw,
		CreatedAt:      time.Now(),
	}
	if err := deps.Store.UpsertValidation(ctx, rec); err != nil {
		deps.Logger.Error("validation record persist failed",
			"scene_id", sceneID.String(), "scene_version", sceneVersion, "err", err)
		return
	}
	deps.Logger.Info("validation campaign complete",
		"scene_id", sceneID.String(), "scene_version", sceneVersion,
		"harness_version", runtime.HarnessVersion, "status", status)

	// R9 lift — arm the live instance on validation success (ADR 006 §3.4
	// path 2, criterion #8). The record now exists, so execForAir resolves
	// the program set; re-loading the roster instance through it both (a)
	// arms an OFF-AIR scene's programs ahead of its later activation and
	// (b) is the DEFERRED SWAP for an ACTIVE scene — once the version
	// validates, it takes the antenna with scene_changed + a fresh
	// snapshot, no second push (completes ADR 003 criterion #15).
	if status == runtime.StatusValidated {
		reloadAfterValidation(ctx, deps, sceneID, sceneVersion, graph, bundle)
	}
}

// reloadAfterValidation re-loads the roster instance with its exec
// installed, but ONLY if the just-validated version is still the scene's
// latest_pushed_version AND the scene is in the roster. The guard avoids
// resurrecting a superseded version (a newer push may have landed during
// the campaign) and avoids loading a scene that was never live. The swap
// is keyed on the SAME validation record execForAir reads — no path
// around #87. If the active scene is the one re-loaded, the swap emits
// scene_changed + a fresh snapshot (the deferred swap, criterion #8).
func reloadAfterValidation(ctx context.Context, deps PublicDeps, sceneID uuid.UUID, sceneVersion string, graph *compiler.Graph, bundle *compiler.RenderBundle) {
	scene, err := deps.Store.GetScene(ctx, sceneID)
	if err != nil {
		deps.Logger.Warn("post-validation reload: scene lookup failed",
			"scene_id", sceneID.String(), "err", err)
		return
	}
	if scene.LatestPushedVersion == nil || *scene.LatestPushedVersion != sceneVersion {
		// A newer version superseded this one during the campaign — do not
		// resurrect the stale version onto the antenna.
		return
	}
	if _, err := deps.Show.Get(sceneID.String()); err != nil {
		// Not in the roster (never loaded / archived): nothing live to
		// arm. A future push or activation will load it through execForAir.
		return
	}

	progs, _, err := execForAir(ctx, deps, sceneID, sceneVersion, graph)
	if err != nil {
		// Fail-closed/loud: a DB error or a corrupt artefact of a version
		// that just validated. Leave the roster instance as it is rather
		// than air an unresolved program set.
		deps.Logger.Error("post-validation reload: execForAir failed",
			"scene_id", sceneID.String(), "scene_version", sceneVersion, "err", err)
		return
	}
	deps.Show.LoadExec(sceneID.String(), graph, bundle, progs...)

	// If this scene is the active one, the swap moves the antenna now: the
	// deferred swap proceeds with scene_changed + a fresh snapshot, just
	// like the mid-broadcast re-push of an already-validated version.
	if active := deps.Show.Active(); active != nil && active.ID() == sceneID.String() {
		active.EmitSceneChanged(sceneID.String(), sceneID.String(), nil)
		active.EmitFreshSnapshot()
	}
}

// computeCampaign builds the report + verdict. Separated so tests drive it
// without the store/goroutine. The B10 guard short-circuits to a failed
// verdict (no campaign runs against a leaky effect registry).
func computeCampaign(deps PublicDeps, graph *compiler.Graph, bundle *compiler.RenderBundle) (runtime.ValidationReport, runtime.ValidationStatus) {
	if err := runtime.ValidateValidationModeCoverage(); err != nil {
		deps.Logger.Error("validation-mode coverage guard failed (B10)", "err", err)
		return runtime.ValidationReport{
			HarnessVersion: runtime.HarnessVersion,
			Status:         runtime.StatusFailed,
		}, runtime.StatusFailed
	}
	progs, err := runtime.ExecProgramsFromGraph(graph)
	if err != nil {
		deps.Logger.Error("exec program decode failed; scene fails validation", "err", err)
		return runtime.ValidationReport{
			HarnessVersion: runtime.HarnessVersion,
			Status:         runtime.StatusFailed,
		}, runtime.StatusFailed
	}
	report := deps.Harness.Validate(graph, bundle, progs)
	return report, report.Status
}

// getValidation handles GET /api/v1/scenes/{id}/validation?v={version}.
// Without ?v= it resolves the latest pushed version. Returns the status +
// report, or 404 when no campaign has run for that (scene, version,
// harness).
func getValidation(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sceneID, ok := parseUUID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene id"})
			return
		}
		version := r.URL.Query().Get("v")
		if version == "" {
			pv, err := deps.Store.GetLatestPushedVersion(r.Context(), sceneID)
			if err != nil {
				status, code := codeFromError(err)
				writeJSON(w, status, map[string]string{"code": code})
				return
			}
			version = pv.SceneVersion
		}
		rec, err := deps.Store.GetValidation(r.Context(), sceneID, version, runtime.HarnessVersion)
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"code":            "VALIDATION_NOT_FOUND",
				"scene_version":   version,
				"harness_version": runtime.HarnessVersion,
			})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"scene_id":        sceneID.String(),
			"scene_version":   rec.SceneVersion,
			"harness_version": rec.HarnessVersion,
			"status":          rec.Status,
			"report":          rec.Report,
		})
	}
}
