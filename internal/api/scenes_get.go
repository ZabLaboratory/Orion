package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// pgxTx is a local alias so we don't name pgx.Tx in handler bodies.
type pgxTx = pgx.Tx

// getRenderBundle serves /api/v1/scenes/{id}/render-bundle?v={hash}.
// Cacheable forever by hash (immutable artefact). Without ?v= the
// latest pushed version is served and Cache-Control is short.
func getRenderBundle(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveCompiledArtefact(w, r, deps, func(pv *store.ScenePushedVersion) (json.RawMessage, *compiler.RenderBundle) {
			return pv.BundleJSON, nil
		})
	}
}

// getOperatorInputs serves the same operator_inputs slice from the
// bundle to non-Solar adapters (Companion, mPrism). The render
// bundle's bytes already carry the slice; we extract just that slice
// to keep payload tight.
func getOperatorInputs(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pv, code, err := resolvePushedVersion(r.Context(), deps, r)
		if err != nil {
			writeJSON(w, code, map[string]string{"code": "PUSHED_VERSION_NOT_FOUND", "error": err.Error()})
			return
		}
		var bundle compiler.RenderBundle
		if err := json.Unmarshal(pv.BundleJSON, &bundle); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		writeImmutable(w, pv.SceneVersion, http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scene_version":    bundle.SceneVersion,
			"operator_inputs":  bundle.OperatorInputs,
		})
	}
}

// getGraph serves the internal graph artefact. Service-only auth.
func getGraph(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Per ADR 004 § 2: service-only.
		// Operator/admin allowed too for debugging.
		// (auth gate is delegated to the trust headers — anyone with
		//  a valid JWT can fetch; tighten later if needed).
		serveCompiledArtefact(w, r, deps, func(pv *store.ScenePushedVersion) (json.RawMessage, *compiler.RenderBundle) {
			return pv.GraphJSON, nil
		})
	}
}

func serveCompiledArtefact(
	w http.ResponseWriter,
	r *http.Request,
	deps PublicDeps,
	pick func(*store.ScenePushedVersion) (json.RawMessage, *compiler.RenderBundle),
) {
	pv, code, err := resolvePushedVersion(r.Context(), deps, r)
	if err != nil {
		writeJSON(w, code, map[string]string{"code": "PUSHED_VERSION_NOT_FOUND", "error": err.Error()})
		return
	}
	body, _ := pick(pv)
	writeImmutable(w, pv.SceneVersion, http.StatusOK)
	_, _ = w.Write(body)
}

// resolvePushedVersion looks up the requested pushed version, either
// pinned via ?v= or implicitly the latest.
func resolvePushedVersion(ctx context.Context, deps PublicDeps, r *http.Request) (*store.ScenePushedVersion, int, error) {
	sceneID, ok := parseUUID(r.PathValue("id"))
	if !ok {
		return nil, http.StatusBadRequest, errors.New("invalid scene id")
	}
	v := r.URL.Query().Get("v")
	if v != "" {
		pv, err := deps.Store.GetPushedVersion(ctx, sceneID, v)
		if err != nil {
			status, _ := codeFromError(err)
			return nil, status, err
		}
		return pv, http.StatusOK, nil
	}
	pv, err := deps.Store.GetLatestPushedVersion(ctx, sceneID)
	if err != nil {
		status, _ := codeFromError(err)
		return nil, status, err
	}
	return pv, http.StatusOK, nil
}

// writeImmutable sets Content-Type, the per-version content hash as
// ETag, and a long-TTL Cache-Control header so CDNs/browsers cache
// the artefact for the lifetime of the version.
func writeImmutable(w http.ResponseWriter, version string, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("ETag", `"`+version+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(status)
}

// postSceneStatus handles POST /api/v1/scenes/{id}/status — archive
// or reactivate. Archiving the active scene is rejected.
func postSceneStatus(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		sceneID, ok := parseUUID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene id"})
			return
		}
		var body struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		switch body.Status {
		case string(store.SceneActive):
			handleReactivate(r.Context(), w, deps, sceneID)
		case string(store.SceneArchived):
			handleArchive(r.Context(), w, deps, sceneID)
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be active|archived"})
		}
	})
}

func handleArchive(ctx context.Context, w http.ResponseWriter, deps PublicDeps, sceneID uuid.UUID) {
	if active := deps.Show.Active(); active != nil && active.ID() == sceneID.String() {
		writeJSON(w, http.StatusConflict, map[string]string{"code": "SCENE_IN_USE"})
		return
	}

	// Purge compiled artefacts + reset latest_pushed_version, then
	// flip status — atomic.
	err := deps.Store.Tx(ctx, func(tx pgxTx) error {
		if _, err := deps.Store.PurgePushedVersions(ctx, tx, sceneID); err != nil {
			return err
		}
		var nullPtr *string
		return deps.Store.SetLatestPushedVersion(ctx, tx, sceneID, nullPtr)
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL", "error": err.Error()})
		return
	}
	if err := deps.Store.SetSceneStatus(ctx, sceneID, store.SceneArchived); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL", "error": err.Error()})
		return
	}
	deps.Show.Unload(sceneID.String())
	writeJSON(w, http.StatusOK, map[string]string{
		"status":                  string(store.SceneArchived),
		"latest_pushed_version":   "",
	})
}

func handleReactivate(ctx context.Context, w http.ResponseWriter, deps PublicDeps, sceneID uuid.UUID) {
	if err := deps.Store.SetSceneStatus(ctx, sceneID, store.SceneActive); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": string(store.SceneActive)})
}
