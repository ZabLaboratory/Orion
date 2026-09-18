package api

import (
	"encoding/json"
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

// getRenderBundle serves /api/v1/scenes/{id}/render-bundle?v={hash}.
//
// Migrated off Store (#15, #331): unlike the legacy Store-backed archive
// of every pushed version ever, bluehost only holds what's CURRENTLY
// loaded — no historical/rolled-back version is servable. Accepted
// because a live scene never changes version without a fresh ZabCanvas
// push, and Prism always sends the current scene on every switch, so
// there is no real path that needs an old version.
//
// {id} is NO LONGER vestigial (F1, Blue#345 / R13): it is required and
// matched, together with ?v=, against whichever bluehost.Host slot
// (on-air first, then preview) is serving that exact (scene_id, digest)
// pair — see resolveHostBundle's doc for the harvest this closes and the
// known gap it does not.
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

// postSceneStatus (archive/reactivate) — RETIRED (#15, #331): archiving
// purged Store-persisted artefacts and flipped a Store-persisted status
// column, neither of which exists anymore. No equivalent concept exists
// in the new model (a bluehost slot is either loaded or not; there is no
// scene registry to archive an entry FROM).

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

// resolveHostBundle finds the bundle bytes matching BOTH {id} and ?v=
// across both bluehost slots, on-air first — the on-air instance is the
// one actually airing, so it wins a tie when both slots happen to share
// the same digest.
//
// F1 (Blue#345 / R13 VETO 1, Bastion): both keys are now REQUIRED, and
// there is no fallback. Before this, an absent or empty ?v= fell back to
// "the first non-empty slot, on-air first" — an unauthenticated caller
// who knew nothing but a syntactically valid {id} got the live antenna
// bundle back verbatim (ZabGate's `/lsdp/v1/scenes/{anything}/bundle`
// public prefix reaches this resolver with no auth at all). The door is
// now capability-by-address only: a caller must already know the exact
// digest this slot is serving AND the scene_id it was prepared/taken
// for — "give me what's live" is no longer a request this resolver
// answers.
//
// host.Serving(slot, id, v) is the SAME (sceneID, digest) identity check
// bluehost.Host already uses for its own idempotent-Prepare admission
// (host.go) — no new identity concept invented here. Serves all three
// consumers of this resolver identically (render-bundle, lsml-bundle,
// operator-inputs — getOperatorInputs calls it directly), so none of the
// three keeps the wider door open behind the other two.
//
// KNOWN GAP, NOT CLOSED HERE (clause 5, Amendment 3 territory,
// bluehost/host.go:478-499): Host.Take never records a sceneID — only
// Prepare does — so host.Serving(SlotOnAir, id, v) can only ever be true
// for id=="". No legitimate on-air fetch through THIS route currently
// succeeds anyway regardless of {id}/?v= — see the PR: the client-side
// version check in @lumencast/runtime independently refuses every
// response this branch could produce, because no occupation carries a
// non-empty scene_version (clause 11's second half, also not closed
// here). This fix closes the harvest (the ONLY consumer that extracted a
// usable result from the prior fallback); it does not — and cannot,
// without touching the reserved slot-identity surface — make an on-air
// fetch through this resolver succeed. Diagnostic gain (silent client
// failure → explicit 404), not a rendering fix.
func resolveHostBundle(deps PublicDeps, r *http.Request) (digest string, bundle []byte, ok bool) {
	sceneID := r.PathValue("id")
	v := r.URL.Query().Get("v")
	if sceneID == "" || v == "" {
		return "", nil, false
	}
	// Preserve the existing bluehost/Program precedence byte-for-byte. The
	// Preview fallback is additive and is consulted only when neither host
	// slot serves the exact content address.
	if deps.SceneIntent != nil && deps.SceneIntent.Host != nil {
		host := deps.SceneIntent.Host
		for _, slot := range []bluehost.Slot{bluehost.SlotOnAir, bluehost.SlotPreview} {
			if !host.Serving(slot, sceneID, v) {
				continue
			}
			b := host.Bundle(slot)
			if b == nil {
				continue
			}
			return v, b, true
		}
	}
	if deps.Preview != nil {
		if b, found := deps.Preview.Bundle(sceneID, v); found {
			return v, b, true
		}
	}
	// Embedded-local callers may wire the editable authoring lane separately
	// from the regular preview slot. Keep the exact (scene,version) lookup
	// scoped to that slot as a compatibility fallback; it never consults the
	// Program/antenne host.
	if deps.EditablePreview != nil && deps.EditablePreview != deps.Preview {
		if b, found := deps.EditablePreview.Bundle(sceneID, v); found {
			return v, b, true
		}
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
