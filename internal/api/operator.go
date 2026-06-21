package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// The operator runtime surface (Orion #209, Blue ADR 008 §3.2/§3.3): the
// three routes through which a broadcast operator drives a live blueprint's
// exec graph — fire a named on-call entrypoint, list the graph's suspension
// points, and resolve one with a value.
//
// AUTHZ: every route is operator-or-admin (requireOperator) — these inject
// a value INTO a running graph at the antenna, never a read-only surface
// (the pending listing is operator-only too: it reveals the live UI prompts).
//
// ACTIVE-ONLY (ADR 008 invariant): all three operate against Show.Active()
// ONLY. A blueprint that is not part of the active scene is dormant — its
// entrypoints cannot be called and its awaits do not exist — answered 409
// (call) / 410 (resolve) / empty (pending). This matches ADR Orion 008:
// only the active scene executes, so only its operator surface is live.
//
// LIMITS: request bodies are bounded (maxOperatorBody); the path params are
// taken verbatim as the scene-local blueprint key / entrypoint id / await
// name (no interpolation, no SQL, no shell — they only key in-memory maps).

// maxOperatorBody bounds the call/resolve request body read.
const maxOperatorBody = 64 << 10

// defaultBlueprintToken is the addressing token the operator routes and the
// cockpit contract use to name the DEFAULT (legacy single / blueprint-free)
// blueprint of a scene, whose real scene-local key is the empty string ""
// (ADR 001 §3.2 — the legacy key, byte-identical leaf paths; compiler/
// blueprints.go). The empty key cannot ride a path segment: Go 1.22 ServeMux
// never matches an empty `{blueprint_id}` segment, so a cockpit that emitted
// `blueprint_id:""` produced `POST /operator/call//on_lck` → an unroutable
// 404, leaving a legacy scene's on-call unreachable over HTTP even though the
// runtime fire (FireOnCall) worked (e2e #152 gap, ADR 016 RC-6).
//
// The fix is an API-boundary alias ONLY: `/cockpit/contracts` emits `_` for
// the empty key, and the operator routes decode `_` back to "" before any
// runtime call (resolveBlueprintKey). The runtime keying is unchanged — the
// scene's exec program is still keyed "" — so the invariant local==antenne
// holds (same route, same contract, same resolution everywhere). `_` is
// reserved: the compiler rejects an authored non-empty blueprint key equal to
// `_` (compiler RESERVED_BLUEPRINT_KEY), so the alias is unambiguous.
const defaultBlueprintToken = "_"

// resolveBlueprintKey maps the HTTP addressing token to the runtime
// scene-local blueprint key. The default token `_` resolves to the empty
// (legacy/default) key; every other value passes through verbatim. This is the
// single decode point the three operator routes share.
func resolveBlueprintKey(token string) string {
	if token == defaultBlueprintToken {
		return ""
	}
	return token
}

// operatorCallBody is the POST /operator/call body: the payload bound under
// the on-call node's data-out pin. Absent/empty body = null payload.
type operatorCallBody struct {
	Payload json.RawMessage `json:"payload,omitempty"`
}

// operatorResolveBody is the POST /operator/resolve body: the value supplied
// to the await, type-checked against its value_type.
type operatorResolveBody struct {
	Value json.RawMessage `json:"value"`
}

// postOperatorCall handles POST /api/v1/operator/call/{blueprint_id}/{entrypoint_id}.
// Fires the named on-call entrypoint of the active scene's blueprint with the
// request payload. 409 if the blueprint is not active or the entrypoint is
// unknown/not an on-call (dormant — ADR 008 active-only).
func postOperatorCall(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		blueprintID := resolveBlueprintKey(r.PathValue("blueprint_id"))
		entrypointID := r.PathValue("entrypoint_id")

		active := deps.Show.Active()
		if active == nil || !active.HostsBlueprint(blueprintID) {
			writeOperatorError(w, http.StatusConflict, "BLUEPRINT_NOT_ACTIVE",
				"blueprint is not part of the active scene")
			return
		}
		entryKey := blueprintID + "/" + entrypointID
		if !active.HasOnCallEntry(entryKey) {
			writeOperatorError(w, http.StatusConflict, "ENTRYPOINT_UNKNOWN",
				"no on-call entrypoint by that id on the active blueprint")
			return
		}

		var body operatorCallBody
		if !readOperatorBody(w, r, &body) {
			return
		}
		if !active.FireOnCall(entryKey, body.Payload) {
			writeOperatorError(w, http.StatusServiceUnavailable, "SCENE_BUSY",
				"scene inbox full — fire not accepted")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "fired"})
	})
}

// getRuntimePending handles GET /api/v1/runtime/{blueprint_id}/pending.
// Lists the live operator awaits of the active scene's blueprint. An
// inactive blueprint has no awaits (empty list — ADR 008 active-only).
func getRuntimePending(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		blueprintID := resolveBlueprintKey(r.PathValue("blueprint_id"))
		pending := []runtime.PendingAwait{}
		if active := deps.Show.Active(); active != nil && active.HostsBlueprint(blueprintID) {
			pending = active.PendingAwaits(blueprintID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"pending": pending})
	})
}

// postOperatorResolve handles
// POST /api/v1/operator/resolve/{blueprint_id}/{await_name}. Supplies a
// value to a live await, type-checked against its value_type, resuming the
// suspended continuation. 410 if no live await matches (never armed, already
// resolved, or invalidated by a switch-away — ADR 008 invariant 7); 422 on a
// type mismatch.
func postOperatorResolve(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		blueprintID := resolveBlueprintKey(r.PathValue("blueprint_id"))
		awaitName := r.PathValue("await_name")

		var body operatorResolveBody
		if !readOperatorBody(w, r, &body) {
			return
		}
		if len(body.Value) == 0 {
			writeOperatorError(w, http.StatusBadRequest, "VALUE_REQUIRED",
				"resolve body must carry a `value`")
			return
		}

		active := deps.Show.Active()
		if active == nil || !active.HostsBlueprint(blueprintID) {
			// Dormant / unknown blueprint: the await cannot exist — Gone.
			writeOperatorError(w, http.StatusGone, "AWAIT_GONE",
				"no live await for this blueprint (inactive or invalidated)")
			return
		}
		err := active.ResolveAwait(blueprintID, awaitName, body.Value)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
		case errors.Is(err, runtime.ErrAwaitTypeMismatch):
			writeOperatorError(w, http.StatusUnprocessableEntity, "VALUE_TYPE_MISMATCH",
				"value does not match the await's value_type")
		case errors.Is(err, runtime.ErrAwaitUnknown):
			writeOperatorError(w, http.StatusGone, "AWAIT_GONE",
				"no live await by that name (already resolved or invalidated)")
		default:
			writeOperatorError(w, http.StatusInternalServerError, "INTERNAL", "resolve failed")
		}
	})
}

// readOperatorBody reads a bounded JSON body into dst. An empty body is
// tolerated (dst left zero — call's null payload). A malformed body is a
// 400. Returns false (and writes the error) on failure.
func readOperatorBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxOperatorBody))
	if err != nil {
		writeOperatorError(w, http.StatusBadRequest, "BAD_BODY", "could not read request body")
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if json.Unmarshal(raw, dst) != nil {
		writeOperatorError(w, http.StatusBadRequest, "BAD_BODY", "malformed JSON body")
		return false
	}
	return true
}

// writeOperatorError writes a typed Orion error envelope (conventions.md:
// Go services use typed error codes, not RFC 7807).
func writeOperatorError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
