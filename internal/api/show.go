package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// getShow returns the show summary: active scene id, scene roster,
// connected client count.
func getShow(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var activeID string
		if a := deps.Show.Active(); a != nil {
			activeID = a.ID()
		}
		body := map[string]any{
			"active_scene_id": activeID,
			"scenes":          deps.Show.IDs(),
		}
		writeJSON(w, http.StatusOK, body)
	}
}

// postActiveScene flips the active-scene pointer.
// Body: {"scene_id": "...", "transition": {kind: "...", duration_ms: ...}}
func postActiveScene(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SceneID    string          `json:"scene_id"`
			Transition json.RawMessage `json:"transition,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if _, ok := parseUUID(body.SceneID); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene_id"})
			return
		}

		// Reject scenes that have never been pushed: criterion 2.
		scene, err := deps.Store.GetScene(r.Context(), uuid.MustParse(body.SceneID))
		if err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}
		if scene.LatestPushedVersion == nil {
			writeJSON(w, http.StatusConflict, map[string]string{"code": "SCENE_NOT_PUSHED"})
			return
		}

		// Validation gate (ADR 003 §3.2.2, B3 — critical). Refuse to put a
		// version on air that has no `validated` record for the current
		// harness_version. Fail-closed on a DB error: an unproven version
		// never reaches the antenna.
		eligible, err := isAirEligible(r.Context(), deps, uuid.MustParse(body.SceneID), *scene.LatestPushedVersion)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		if !eligible {
			writeJSON(w, http.StatusConflict, map[string]string{
				"code":          sceneNotValidatedCode,
				"scene_version": *scene.LatestPushedVersion,
			})
			return
		}

		if err := deps.Show.SetActive(body.SceneID, body.Transition); err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"active_scene_id": body.SceneID})
	})
}

// postTestSession opens a fresh test session for a given scene.
// Body: {"scene_id": "..."}
func postTestSession(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SceneID string `json:"scene_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		_ = runtime.Subscription{} // keep runtime import live (placeholder usage)

		scene, err := deps.Show.Get(body.SceneID)
		if err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}
		// No exec programs passed: the live-activation seam (execForAir)
		// does not arm exec yet — exec stays dormant on every API path
		// until the phase-4 gate (ADR 006 §3.4 / issue #106).
		sessionID, _ := deps.Test.Open(r.Context(), body.SceneID, scene.Graph(), scene.Bundle())
		writeJSON(w, http.StatusCreated, map[string]string{
			"session_id": sessionID,
			"ws_url":     "/orion/api/v1/scenes/" + body.SceneID + "/test?session=" + sessionID,
		})
	})
}
