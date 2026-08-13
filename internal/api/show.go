package api

import (
	"encoding/json"
	"net/http"

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

// postActiveScene (POST /show/active-scene) — RETIRED (#15, #331). Store-
// dependent end to end (GetScene, isAirEligible/execForAir, SetActiveSceneID,
// loadSceneFromStore), plus the state_snapshot IMPORT path forbidden outright
// by ADR-BLUE-012 invariant #8 (a client-supplied payload may never
// initialize/mutate an instance). No equivalent is implemented in Orion: the
// scene-intent path (attestation-driven, deps.SceneIntent) supersedes it.
//
// BREAKS Prism: `Prism/src/main/embedded-boot.ts:172` and
// `Prism/src/renderer/src/lib/scene-push.ts` call this route as part of the
// go-live pipeline. Migrating Prism to scene-intent is a separate work
// stream on the Prism repo, out of scope here — see the #331 final report.

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
		// Test sessions are NEVER gated (ADR 006 §3.4): the author iterates
		// freely, so exec runs in a test session WITHOUT a validation
		// record. The session installs the scene's full program set from
		// its compiled graph directly (no execForAir, no #87 gate) — the
		// exec-quiescence on-air flag governs only LIVE roster instances,
		// never a session clone (TestSessionManager.Open leaves it ungated).
		progs, err := runtime.ExecProgramsFromGraph(scene.Graph())
		if err != nil {
			// A corrupt exec artefact: fail-loud rather than open a session
			// whose exec is silently dead.
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		sessionID, _ := deps.Test.Open(r.Context(), body.SceneID, scene.Graph(), scene.Bundle(), progs...)
		resp := map[string]string{
			"session_id": sessionID,
			"ws_url":     "/orion/api/v1/scenes/" + body.SceneID + "/test?session=" + sessionID,
		}
		// In dual/lsdp mode the session also exposes an isolated LSDP wire
		// (the preview Solar runtime is LSDP-only). It follows ONLY this
		// session's clone, never the antenne's active scene.
		if deps.LSDPHandler != nil {
			resp["lsdp_ws_url"] = "/orion/api/v1/scenes/" + body.SceneID + "/test.lsdp?session=" + sessionID
		}
		writeJSON(w, http.StatusCreated, resp)
	})
}

// testSessionLSDP serves the per-session preview LSDP/1.1 wire. Operator/
// admin only (parity with the bespoke ServeTestSession gate). It marks the
// session WS-active, delegates the upgrade to the session's OWN kit server
// (rewriting the path onto the kit's /lsdp.v1, mirroring lsdpRoute), and
// arms the grace window when the socket closes. The kit re-derives identity
// from the same trust headers via Config.IdentityFromRequest.
func testSessionLSDP(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("session")
		if sessionID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session id required"})
			return
		}
		handler, err := deps.Test.ConnectWire(sessionID)
		if err != nil {
			writeJSON(w, http.StatusGone, map[string]string{"code": "TEST_SESSION_EXPIRED"})
			return
		}
		defer deps.Test.Disconnect(sessionID)
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/lsdp.v1"
		handler.ServeHTTP(w, r2)
	})
}

// postPreviewActiveScene flips the PERSISTENT preview wire to scene_id — the
// preview/antenne split (working model). The scene must already be loaded in
// the show (the cockpit pushes+validates+re-pushes before this), so we clone
// its compiled graph+bundle into a fresh isolated preview instance and swap
// the preview wire's active clone to it. The antenne (global show active
// scene + /show/stream.lsdp) is NEVER touched — a preview switch can no longer
// flip the live antenne. exec runs UNGATED (author-facing preview, no R9 gate),
// exactly as a test session. Operator-gated; 404 in bespoke mode (no preview
// wire).
func postPreviewActiveScene(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if deps.Preview == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		var body struct {
			SceneID string `json:"scene_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		scene, err := deps.Show.Get(body.SceneID)
		if err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}
		// Install the scene's full exec program set on the clone (ADR 003
		// §3.1; ungated in preview, mirroring postTestSession). A corrupt
		// exec artefact fails loud rather than airing a silently-dead preview.
		progs, err := runtime.ExecProgramsFromGraph(scene.Graph())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
			return
		}
		deps.Preview.Activate(body.SceneID, scene.Graph(), scene.Bundle(), progs...)
		writeJSON(w, http.StatusOK, map[string]string{"scene_id": body.SceneID})
	})
}

// getPreviewSnapshot exports the live preview clone's state for the
// preview→air hand-off. Since the preview prep now lives in the isolated
// preview slot (the global show no longer runs the preview scene), the
// hand-off must read it here — the show would return blank/stale defaults.
// Sidecar/embedded-local only (parity with getStateSnapshot VETO #11: a
// prod/antenne Orion never exports arbitrary scene state). An empty slot is a
// non-fatal absence (the push proceeds from defaults), returned as an empty
// snapshot rather than an error.
func getPreviewSnapshot(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, _ *http.Request) {
		if !deps.Config.Profile.IsEmbeddedLocal() {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		if deps.Preview == nil {
			writeJSON(w, http.StatusOK, stateSnapshot{Seq: 0, State: map[string]json.RawMessage{}})
			return
		}
		version, seq, state, ok := deps.Preview.SnapshotState()
		if !ok {
			writeJSON(w, http.StatusOK, stateSnapshot{Seq: 0, State: map[string]json.RawMessage{}})
			return
		}
		writeJSON(w, http.StatusOK, stateSnapshot{Version: version, Seq: seq, State: state})
	})
}
