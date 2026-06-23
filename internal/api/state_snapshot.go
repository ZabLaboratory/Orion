package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ZabLaboratory/Orion/internal/auth"
)

// Preview→air state hand-off seams (ADR Prism 005 Amendment 2 §A2.2.d/e,
// Orion issue #256). Two endpoints carry the runtime state a scene built
// up in the preview sidecar to the SAME scene as it takes the antenna in
// prod, so the live scene starts with the prepared state (resolved awaits,
// preloaded operator values) rather than a virgin one:
//
//   (a) GET  /scenes/{id}/state-snapshot — export from the sidecar.
//   (b) POST /show/active-scene { state_snapshot } — seed-then-activate
//       in prod (the snapshot field is wired into postActiveScene, not
//       here; this file owns the fail-closed validation it calls).
//
// The import side (b) writes state into a PROD scene, so it is a hostile
// surface. Every Bastion VETO is enforced in the HANDLER, BEFORE any Seed
// — never inside runtime.State.Seed, which writes whatever it is handed.

// snapshotMaxBytes caps the request body of the seed seam (Bastion #8,
// fail-closed size bound). A scene's whole leaf state is small (scalars +
// short strings); 1 MiB is multiple orders of magnitude of headroom while
// refusing a memory-exhaustion body outright.
const snapshotMaxBytes = 1 << 20

// snapshotMaxPaths caps the number of leaf paths a snapshot may carry
// (Bastion #8, second leg of the size bound: a body can be small yet carry
// pathologically many keys). Far above any real scene's leaf count.
const snapshotMaxPaths = 4096

// Refusal codes (stable wire contract; ADR Prism 005 §A2.4/§A2.5).
const (
	snapshotPathUnknownCode     = "SNAPSHOT_PATH_UNKNOWN"     // VETO #1 + #2
	snapshotVersionMismatchCode = "SNAPSHOT_VERSION_MISMATCH" // VETO #5 (R11)
	snapshotMalformedCode       = "SNAPSHOT_MALFORMED"        // #6
	snapshotTooLargeCode        = "SNAPSHOT_TOO_LARGE"        // VETO #8
	sceneIsLiveCode             = "SCENE_IS_LIVE"             // VETO #3
)

// stateSnapshot is the wire shape of the hand-off, identical on export (a)
// and import (b). `version` is carried IN the snapshot (Bastion #5): the
// import seam compares it == the target's air-eligible version, never
// deduces it. `state` is per-leaf raw JSON: each value stays a
// json.RawMessage so well-formedness is checked per-leaf (Bastion #6)
// without the snapshot's values ever being interpreted here.
type stateSnapshot struct {
	Version string                     `json:"version"`
	Seq     uint64                     `json:"seq,omitempty"`
	State   map[string]json.RawMessage `json:"state"`
}

// getStateSnapshot serves seam (a): export the live state of a scene
// loaded on the PREVIEW sidecar (ADR Prism 005 §A2.2.d).
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

// reservedNamespacePrefixes are the engine/platform leaf prefixes the
// hand-off must NEVER transport (Bastion VETO #2): even if such a path
// somehow appeared in a scene's keyspace, seeding it would let a forged
// snapshot inject e.g. a fabricated Twitch event at the antenna. The
// hand-off carries ONLY author-declared scene state. A path equal to, or
// under, any of these is rejected outright — the whole snapshot, no Seed.
var reservedNamespacePrefixes = []string{
	"__system",
	"__events",
	"__inputs",
	"__vars",
	"__test",
	"__resolved_source",
}

// isReservedPath reports whether a leaf path falls in a reserved
// engine/platform namespace (Bastion VETO #2). It matches the exact
// segment or a dotted descendant, so `__events` and `__events.foo` are both
// reserved while a hypothetical author path `__eventsmaybe` is not falsely
// caught (segment-boundary aware).
func isReservedPath(path string) bool {
	for _, p := range reservedNamespacePrefixes {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	// Defence in depth: any leaf whose first segment is itself an engine
	// double-underscore namespace is reserved, even if not enumerated
	// above. Author state never starts with `__`.
	if strings.HasPrefix(path, "__") {
		return true
	}
	return false
}

// validateSnapshotForSeed runs the FULL fail-closed validation of an import
// snapshot against the target scene, returning a sanitised copy to Seed ONLY
// if every check passes (Bastion VETO #4: reject-before-effect / atomicity).
// It performs ZERO mutation — the caller seeds the returned map, or refuses.
//
//	snap          — the decoded snapshot (version carried within, #5).
//	targetVersion — the target's air-eligible LatestPushedVersion (#5/R11).
//	keyspace      — the target version's author-declared leaf keyspace (#1).
//
// Checks, all before any Seed (#4):
//   - #5  version carried in snapshot == target air-eligible version.
//   - #8  path count cap (body size already bounded by MaxBytesReader).
//   - #2  no reserved/engine namespace path.
//   - #1  every path ∈ declared keyspace, fail-closed (unknown ⇒ reject all).
//   - #6  every value is well-formed JSON (json.RawMessage validity).
//
// On the first failure it returns (nil, code, false): the caller writes the
// refusal and seeds NOTHING. The returned map is a fresh copy — Seed never
// sees the request-controlled map header.
func validateSnapshotForSeed(snap *stateSnapshot, targetVersion string, keyspace map[string]struct{}) (clean map[string]json.RawMessage, code string, ok bool) {
	// #5 / R11: strict version match. No deduction — the snapshot must
	// declare the exact air-eligible version of the target.
	if snap.Version == "" || snap.Version != targetVersion {
		return nil, snapshotVersionMismatchCode, false
	}
	// #8: path-count cap (second leg of the size bound).
	if len(snap.State) > snapshotMaxPaths {
		return nil, snapshotTooLargeCode, false
	}
	clean = make(map[string]json.RawMessage, len(snap.State))
	for path, raw := range snap.State {
		// #2: reserved namespace — rejected even if (somehow) in keyspace.
		if isReservedPath(path) {
			return nil, snapshotPathUnknownCode, false
		}
		// #1: keyspace whitelist, fail-closed. Unknown path ⇒ reject the
		// WHOLE snapshot, no partial Seed.
		if _, declared := keyspace[path]; !declared {
			return nil, snapshotPathUnknownCode, false
		}
		// #6: per-leaf well-formedness. A nil/empty or syntactically
		// invalid value is a malformed snapshot — refuse, do not Seed.
		if len(raw) == 0 || !json.Valid(raw) {
			return nil, snapshotMalformedCode, false
		}
		// Copy the bytes so the seeded value is independent of the decoded
		// request body (which the caller may discard / reuse).
		cp := make(json.RawMessage, len(raw))
		copy(cp, raw)
		clean[path] = cp
	}
	return clean, "", true
}

// snapshotRoleRefused reports whether an identity's role is barred from the
// import seam (Bastion #9, tranché): operator-only — the `service` role is
// explicitly refused on (b) (no service calls it in prod), and so is every
// non-operator/admin role. operatorGate already refuses everything except
// operator/admin; this is the explicit, test-anchored statement of the
// service refusal for the record.
func snapshotRoleRefused(role auth.Role) bool {
	return role != auth.RoleOperator && role != auth.RoleAdmin
}
