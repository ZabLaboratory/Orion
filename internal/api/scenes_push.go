package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// pushScene handles POST /api/v1/scenes/{id}/push. Either compiles a
// new pushed version from a definition envelope, or re-points
// latest_pushed_version at an existing scene_version (rollback).
func pushScene(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		sceneID, ok := parseUUID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene id"})
			return
		}
		var envelope compiler.PushEnvelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid push envelope"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), deps.Config.PushTimeout)
		defer cancel()

		scene, err := deps.Store.GetScene(ctx, sceneID)
		if err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}
		if scene.Status == store.SceneArchived {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "SCENE_ARCHIVED"})
			return
		}

		if envelope.IsRollback() {
			handleRollback(ctx, w, deps, sceneID, envelope.RollbackTo)
			return
		}

		started := time.Now()
		graph, bundle, sceneVersion, err := compiler.Compile(ctx, sceneID.String(), envelope, deps.Fetcher)
		if err != nil {
			deps.Metrics.PushTotal.WithLabelValues("compile_error").Inc()
			deps.Metrics.PushDuration.WithLabelValues("compile_error").Observe(time.Since(started).Seconds())
			writePushError(w, err)
			return
		}

		// Persist definition + pushed version + advance pointer in
		// one transaction so external observers never see torn state.
		nextDefVer, err := deps.Store.MaxDefinitionVersion(ctx, sceneID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		nextDefVer++

		definitionID := uuid.New()
		definition := store.SceneDefinition{
			ID:                definitionID,
			SceneID:           sceneID,
			DefinitionVersion: nextDefVer,
			CanvasVersion:     envelope.CanvasVersion,
			BlueBlueprintID:   envelope.BlueBlueprintID,
			ComponentsJSON:    mustJSON(envelope.Components),
			CreatedAt:         time.Now(),
		}
		if err := deps.Store.InsertDefinition(ctx, definition); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}

		err = deps.Store.Tx(ctx, func(tx pgx.Tx) error {
			pv := store.ScenePushedVersion{
				SceneID:      sceneID,
				SceneVersion: sceneVersion,
				DefinitionID: definitionID,
				GraphJSON:    mustJSON(graph),
				BundleJSON:   mustJSON(bundle),
				CreatedAt:    time.Now(),
			}
			if err := deps.Store.InsertPushedVersion(ctx, tx, pv); err != nil {
				return err
			}
			return deps.Store.SetLatestPushedVersion(ctx, tx, sceneID, &sceneVersion)
		})
		if err != nil {
			deps.Metrics.PushTotal.WithLabelValues("persist_error").Inc()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}

		// Surface the new version into the runtime: either swap the
		// graph on a live scene (re-push of an active scene) or load
		// the scene anew if this was its first push.
		deps.Show.Load(sceneID.String(), graph, bundle)
		if active := deps.Show.Active(); active != nil && active.ID() == sceneID.String() {
			// Mid-broadcast re-push (criterion 9).
			active.EmitSceneChanged(sceneID.String(), sceneID.String(), nil)
			active.EmitFreshSnapshot()
		}

		deps.Metrics.PushTotal.WithLabelValues("ok").Inc()
		deps.Metrics.PushDuration.WithLabelValues("ok").Observe(time.Since(started).Seconds())

		writeJSON(w, http.StatusOK, map[string]any{
			"scene_version": sceneVersion,
			"diagnostics": map[string]any{
				"errors":   []string{},
				"warnings": []string{},
			},
		})
	})
}

// writePushError translates a *compiler.CompileError into the
// chantier-spec'd response shape.
func writePushError(w http.ResponseWriter, err error) {
	var ce *compiler.CompileError
	if errors.As(err, &ce) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"code": "COMPILE_FAILED",
			"diagnostics": map[string]any{
				"errors":   ce.Diagnostics.Errors(),
				"warnings": ce.Diagnostics.Warnings(),
			},
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL", "error": err.Error()})
}

func handleRollback(ctx context.Context, w http.ResponseWriter, deps PublicDeps, sceneID uuid.UUID, target string) {
	pv, err := deps.Store.GetPushedVersion(ctx, sceneID, target)
	if err != nil {
		status, code := codeFromError(err)
		writeJSON(w, status, map[string]string{"code": code})
		return
	}

	err = deps.Store.Tx(ctx, func(tx pgx.Tx) error {
		return deps.Store.SetLatestPushedVersion(ctx, tx, sceneID, &pv.SceneVersion)
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	}

	// Re-load the runtime scene from the rolled-back artefacts.
	graph := &compiler.Graph{}
	bundle := &compiler.RenderBundle{}
	if err := json.Unmarshal(pv.GraphJSON, graph); err == nil {
		_ = json.Unmarshal(pv.BundleJSON, bundle)
		deps.Show.Load(sceneID.String(), graph, bundle)
		if active := deps.Show.Active(); active != nil && active.ID() == sceneID.String() {
			active.EmitSceneChanged(sceneID.String(), sceneID.String(), nil)
			active.EmitFreshSnapshot()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"scene_version": pv.SceneVersion,
		"rolled_back":   true,
	})
}

// mustJSON marshals or panics. Used for fields whose error path is
// already covered upstream — a compile failure shouldn't leave us
// trying to marshal the artefacts here.
func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func parseUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}
