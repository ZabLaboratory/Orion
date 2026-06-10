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
// scene reach air; this file does NOT wire any ExecProgram emission into
// the live activation path (R9): the campaign proves the programs the
// artefact carries, the enforcement refuses unproven versions, and nothing
// here installs a program onto a live scene.

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
