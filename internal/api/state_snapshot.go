package api

import (
	"encoding/json"
	"net/http"
)

// Preview→air state hand-off, EXPORT side only (ADR Prism 005 Amendment 2
// §A2.2.d, Orion issue #256): GET /scenes/{id}/state-snapshot reads the
// runtime state a scene built up in the preview sidecar. Store-independent
// (deps.Test/deps.Show only) — kept through #15/#331.
//
// The IMPORT side (b) — POST /show/active-scene { state_snapshot }, which
// seeded a PROD scene from a client-supplied payload before activating —
// is RETIRED, not migrated: ADR-BLUE-012 invariant #8 explicitly forbids
// a mutable payload/state_snapshot from Prism initializing or mutating an
// instance. That is a deliberate architecture change, not a gap; the
// validation helpers that existed solely for that import path
// (validateSnapshotForSeed, snapshotRoleRefused, isReservedPath, the
// SNAPSHOT_* refusal codes) are removed with postActiveScene.

// stateSnapshot is the wire shape of the export. `state` is per-leaf raw
// JSON so well-formedness stays per-leaf, byte-identical to what the
// scene/session holds.
type stateSnapshot struct {
	Version string                     `json:"version"`
	Seq     uint64                     `json:"seq,omitempty"`
	State   map[string]json.RawMessage `json:"state"`
}

// getStateSnapshot serves the export: the live state of a scene loaded on
// the PREVIEW sidecar.
//
// Bastion #11 (sidecar/preview only): the route refuses outright unless
// this Orion runs the embedded-local (sidecar) profile — a prod/antenne
// Orion never exports arbitrary scene state, which would leak live state
// on a read. Bastion #9/#11: requireOperator gates it (operator/admin
// only; viewer and unauthenticated are refused by operatorGate).
func getStateSnapshot(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		// VETO #11: only the preview sidecar may export state. On antenne
		// this seam does not exist — fail closed with 404 (the route is
		// indistinguishable from absent to a prod caller).
		if !deps.Config.Profile.IsEmbeddedLocal() {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND"})
			return
		}
		id := r.PathValue("id")
		if _, ok := parseUUID(id); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scene_id"})
			return
		}
		// Verrou (per-session LSDP): a ``?session=`` exports the ISOLATED
		// preview test-session's clone, not the global show. Since the preview
		// prep now lives in the session (the sidecar show no longer runs the
		// preview scene), the hand-off export must read it from there — the
		// show would return blank/stale Defaults. A lapsed session answers
		// 410 (TEST_SESSION_EXPIRED), distinct from a real read.
		if sessionID := r.URL.Query().Get("session"); sessionID != "" {
			version, seq, state, err := deps.Test.SnapshotState(sessionID)
			if err != nil {
				writeJSON(w, http.StatusGone, map[string]string{"code": "TEST_SESSION_EXPIRED"})
				return
			}
			writeJSON(w, http.StatusOK, stateSnapshot{Version: version, Seq: seq, State: state})
			return
		}
		scene, err := deps.Show.Get(id)
		if err != nil {
			status, code := codeFromError(err)
			writeJSON(w, status, map[string]string{"code": code})
			return
		}
		version, seq, state := scene.SnapshotState()
		writeJSON(w, http.StatusOK, stateSnapshot{
			Version: version,
			Seq:     seq,
			State:   state,
		})
	})
}
