package api

import (
	"encoding/json"
	"errors"
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
		// Bastion #9 (tranché): operator-only on the seed seam — refuse the
		// `service` role explicitly (no service calls active-scene-with-seed
		// in prod) and every non-operator/admin role. operatorGate already
		// barred all but operator/admin; this re-states the service refusal at
		// the seam that writes prod state. Anti-spoof via authSource: the role
		// can only come from ZabGate's injected header, never the client.
		if snapshotRoleRefused(authSource.FromHeaders(r.Header).Role) {
			http.Error(w, "operator role required", http.StatusForbidden)
			return
		}

		// Bastion #8 (fail-closed size bound): cap the body before decode so
		// a state_snapshot field can never exhaust memory. MaxBytesReader
		// makes Decode error past the cap; surfaced as SNAPSHOT_TOO_LARGE.
		r.Body = http.MaxBytesReader(w, r.Body, snapshotMaxBytes)
		var body struct {
			SceneID       string          `json:"scene_id"`
			Transition    json.RawMessage `json:"transition,omitempty"`
			StateSnapshot *stateSnapshot  `json:"state_snapshot,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "SNAPSHOT_TOO_LARGE"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if _, ok := parseUUID(body.SceneID); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene_id"})
			return
		}

		// Bastion VETO #3 (SCENE_IS_LIVE): a state_snapshot may seed ONLY a
		// scene not yet on air — never the one the viewers are watching
		// (ADR Prism 005 §A2.2.e). Checked here, before ANY effect, so the
		// refusal mutates nothing (RC-A2.3: 0 mutation of the on-screen
		// scene). Re-preparing a live scene = prepare it in preview then
		// re-switch (a fresh activation), never an in-place re-seed.
		if body.StateSnapshot != nil && body.SceneID == deps.Show.ActiveID() {
			writeJSON(w, http.StatusConflict, map[string]string{"code": sceneIsLiveCode})
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

		// Bonus coherence (chantier #4): a scene that is pushed+validated but
		// not yet in the roster (never loaded this process — e.g. activated
		// before its first push landed in-memory, or a stale roster) cannot be
		// SetActive (ErrSceneNotFound → WS `scene not found`). Load it from its
		// validated pushed version first so SetActive always finds it. Load is
		// idempotent: a no-op swap if the scene is already loaded. After the
		// boot fix every active+pushed scene is loaded at startup, so this is a
		// belt-and-braces path, but it aligns activate with push (which also
		// loads on demand) and removes the last "scene not found on activate".
		if _, err := deps.Show.Get(body.SceneID); err != nil {
			if lerr := loadSceneFromStore(r.Context(), deps, uuid.MustParse(body.SceneID)); lerr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
				return
			}
		}

		// Preview→air state hand-off (ADR Prism 005 §A2.2.d, issue #256).
		// When a state_snapshot is present, seed the DESTINATION scene's
		// state BEFORE SetActive (which fires SetOnAir+FireOnStart). The
		// scene is now guaranteed in the roster (load block above).
		//
		// Bastion VETO #4 (atomicity): the FULL fail-closed validation
		// (#1 keyspace, #2 reserved-namespace, #5 version, #6 well-formed,
		// #8 path-count) runs INTEGRALLY before any Seed — on the first
		// failure we refuse with 0 mutation and never reach SetActive. The
		// snapshot is validated against the TARGET's air-eligible version
		// (its declared keyspace), and Seed runs on a sanitised copy only.
		if body.StateSnapshot != nil {
			dest, derr := deps.Show.Get(body.SceneID)
			if derr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
				return
			}
			clean, code, ok := validateSnapshotForSeed(
				body.StateSnapshot,
				*scene.LatestPushedVersion, // #5/R11: the air-eligible version
				dest.DeclaredKeyspace(),    // #1: keyspace of the TARGET version
			)
			if !ok {
				status := http.StatusConflict
				if code == snapshotPathUnknownCode || code == snapshotMalformedCode {
					status = http.StatusBadRequest
				}
				if code == snapshotTooLargeCode {
					status = http.StatusRequestEntityTooLarge
				}
				// Bastion #10: never log the snapshot values — only the
				// scene id, version and verdict.
				deps.Logger.Warn("state snapshot seed refused",
					"scene_id", body.SceneID,
					"snapshot_version", body.StateSnapshot.Version,
					"verdict", code)
				writeJSON(w, status, map[string]string{"code": code})
				return
			}
			// All gates passed: seed before activation (atomic to the screen
			// — the old scene holds the antenna until SetActive swaps).
			dest.SeedState(clean)
			deps.Logger.Info("state snapshot seeded",
				"scene_id", body.SceneID,
				"snapshot_version", body.StateSnapshot.Version,
				"paths", len(clean))
		}

		if err := deps.Show.SetActive(body.SceneID, body.Transition); err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}

		// Persist the antenna selection so it survives a restart/redeploy
		// (the bug this chantier fixes). Best-effort after the in-memory
		// switch: the live antenna already moved; a DB write failure must not
		// 500 a successful on-air switch, but it is logged so a persistence
		// outage is visible (the next boot would then fall back to the prior
		// persisted pointer).
		sid := uuid.MustParse(body.SceneID)
		if err := deps.Store.SetActiveSceneID(r.Context(), &sid); err != nil {
			deps.Logger.Error("persist active scene failed", "scene_id", body.SceneID, "err", err)
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
