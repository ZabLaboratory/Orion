package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// pgxTx is a local alias to the store-neutral transaction handle so we
// don't name the backend tx type in handler bodies (#222 generalised the
// Store tx coupling off pgx).
type pgxTx = store.Tx

// getRenderBundle serves /api/v1/scenes/{id}/render-bundle?v={hash}.
//
// Migrated off Store (#15, #331): the {id} path segment is now VESTIGIAL
// (kept for URL-shape compatibility with Solar/Prism, which hardcode
// this path — porteur decision) and is not resolved against anything;
// the ?v= hash is the real lookup key, matched against whichever
// bluehost.Host slot (on-air first, then preview) currently carries it.
// Porteur's accepted narrowing: unlike the legacy Store-backed archive
// of every pushed version ever, bluehost only holds what's CURRENTLY
// loaded — no historical/rolled-back version is servable. Accepted
// because a live scene never changes version without a fresh ZabCanvas
// push, and Prism always sends the current scene on every switch, so
// there is no real path that needs an old version.
func getRenderBundle(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveHostBundle(w, r, deps)
	}
}

// getOperatorInputs serves the operator_inputs slice from the bundle to
// non-Solar adapters (Companion, mPrism).
//
// Migrated off Store (#15, #331), same posture as getRenderBundle. The
// bundle bytes now come from ZabCanvas's lsml_bundle (§6.3), whose exact
// JSON schema for an "operator_inputs" field has NOT been confirmed
// against Orion's legacy compiler.RenderBundle.OperatorInputs shape —
// unmarshalled here as a generic top-level key, best-effort, rather than
// a typed struct that could silently mismatch. Absent key ⇒ empty array,
// never an error (an authored scene may declare none).
func getOperatorInputs(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		digest, bundle, ok := resolveHostBundle(deps, r)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "PUSHED_VERSION_NOT_FOUND"})
			return
		}
		var envelope map[string]json.RawMessage
		operatorInputs := json.RawMessage(`[]`)
		if err := json.Unmarshal(bundle, &envelope); err == nil {
			if v, ok := envelope["operator_inputs"]; ok {
				operatorInputs = v
			}
		}
		writeImmutable(w, digest, http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scene_version":   digest,
			"operator_inputs": operatorInputs,
		})
	}
}

// getGraph — RETIRED (#15, #331): served Orion's own legacy compiled
// compiler.Graph artifact, which does not exist in the new model at all
// (ZabCanvas/Blue produce blue.program.v1 directly; Orion no longer
// compiles a graph). Verified no caller before removal — no client repo
// (Prism/Solar/mPrism/companion-module) references
// `api/v1/scenes/{id}/graph`; the one "graph" hit in Prism's
// capture-resolver.ts is a ZabCanvas route, unrelated.

// serveHostBundle writes the bundle bytes bluehost.Host currently holds
// for whichever slot matches the request, immutably cacheable by digest.
func serveHostBundle(w http.ResponseWriter, r *http.Request, deps PublicDeps) {
	digest, bundle, ok := resolveHostBundle(deps, r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "PUSHED_VERSION_NOT_FOUND"})
		return
	}
	writeImmutable(w, digest, http.StatusOK)
	_, _ = w.Write(bundle)
}

// resolveHostBundle finds the bundle bytes matching ?v= (if provided)
// across both bluehost slots, on-air first — the on-air instance is the
// one actually airing, so it wins a tie when both slots happen to share
// the same digest. Without ?v=, the first non-empty slot (same order)
// answers, matching legacy's "no ?v= ⇒ latest" default.
func resolveHostBundle(deps PublicDeps, r *http.Request) (digest string, bundle []byte, ok bool) {
	if deps.SceneIntent == nil || deps.SceneIntent.Host == nil {
		return "", nil, false
	}
	host := deps.SceneIntent.Host
	v := r.URL.Query().Get("v")
	for _, slot := range []bluehost.Slot{bluehost.SlotOnAir, bluehost.SlotPreview} {
		d := host.Digest(slot)
		if d == "" {
			continue
		}
		if v != "" && v != d {
			continue
		}
		b := host.Bundle(slot)
		if b == nil {
			continue
		}
		return d, b, true
	}
	return "", nil, false
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
	// A promoted stream rule is in use too (ADR 009 §3.1 criterion #5):
	// archiving it would purge the artefacts of a scene the show runs
	// always. Demote it first.
	if deps.Show.IsStreamRule(sceneID.String()) {
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
		"status":                string(store.SceneArchived),
		"latest_pushed_version": "",
	})
}

func handleReactivate(ctx context.Context, w http.ResponseWriter, deps PublicDeps, sceneID uuid.UUID) {
	if err := deps.Store.SetSceneStatus(ctx, sceneID, store.SceneActive); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": string(store.SceneActive)})
}
