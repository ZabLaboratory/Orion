package lsdp

import (
	"bytes"
	"encoding/json"

	lproto "github.com/Lumencast/lumencast-go/protocol"
	lserver "github.com/Lumencast/lumencast-go/server"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// sceneMirror taps one runtime.Scene's output port and replays it onto
// the paired kit scene. It implements runtime.SceneMirror.
//
// The reactive engine produces typed messages (Snapshot / Delta /
// SceneChanged); this adapter maps each onto the kit's Set / Emit /
// (scene switch is driven from the Show via Wire.SetActive, not here).
// All forwarding is best-effort and non-blocking — the kit's own
// back-pressure (snapshot-collapse) protects slow LSDP subscribers, and
// the bespoke wire remains the source of truth.
type sceneMirror struct {
	wire    *Wire
	sceneID string
	scene   *lserver.Scene
}

var _ runtime.SceneMirror = (*sceneMirror)(nil)

// Forward maps a reactive output message onto the kit scene.
func (m *sceneMirror) Forward(msg runtime.SubscriberMsg) {
	switch v := msg.(type) {
	case *protocol.Snapshot:
		m.scene.SetVersion(v.SceneVersion)
		if len(v.State) == 0 {
			return
		}
		patches := make(map[string]any, len(v.State))
		for path, val := range v.State {
			// LSDP §3.2.1: only scalar / array-of-scalar values are
			// wire-legal. The reactive store also holds db.query
			// intermediates (rows, clause descriptors) as object/array
			// leaves; emitting them makes @lumencast/protocol reject the
			// ENTIRE snapshot (INVALID_VALUE → reconnect loop → black
			// screen). Drop non-scalar leaves here — they are internal
			// compute, never renderable. The bespoke /show/stream wire
			// (ADR 002) is untouched: it does not go through this tap.
			if !isLSDPScalar(val) {
				continue
			}
			patches[path] = val
		}
		if len(patches) == 0 {
			return
		}
		// Set seeds the kit store and re-bases existing kit subscribers
		// with a fresh snapshot — the right semantics for the initial
		// seed and for back-pressure/scene-switch snapshots.
		_ = m.scene.Set(patches)
	case *protocol.Delta:
		if len(v.Patches) == 0 {
			// A zero-patch delta is the bespoke idempotency confirm
			// (ADR 002 §6). The kit has no zero-patch delta concept and
			// Emit rejects empty maps; the LSDP cause-echo for an input
			// is carried by the next non-empty delta, so we drop it.
			return
		}
		patches := make(map[string]any, len(v.Patches))
		for _, p := range v.Patches {
			// Same §3.2.1 filter as the snapshot path: a delta updating a
			// db.query intermediate (e.g. row_k = [{...}]) must not put an
			// object on the LSDP wire. Scalar board/var leaves pass through.
			if !isLSDPScalar(p.Value) {
				continue
			}
			patches[p.Path] = p.Value
		}
		if len(patches) == 0 {
			// Every patch in this delta was a non-scalar intermediate.
			// Nothing wire-legal to emit; Emit rejects empty maps anyway.
			return
		}
		_ = m.scene.EmitWithCause(patches, mapCause(v.Cause))
	case *protocol.SceneChanged:
		// The scene switch is driven authoritatively from the Show via
		// Wire.SetActive (kit Server.SetActive migrates live subs with
		// its own scene_changed + snapshot). Nothing to do per-scene.
	}
}

// isLSDPScalar reports whether a raw-JSON leaf value is legal on the
// LSDP wire per spec §3.2.1: string / number / boolean / null, or an
// array whose elements are (recursively) themselves legal. A JSON
// object anywhere — at the top or nested inside an array — is forbidden
// and makes @lumencast/protocol reject the whole snapshot/delta.
//
// We decode into `any` and walk the shape; this mirrors the decoder's
// recursive assertLeafValue exactly (an array-of-object such as the
// db.query row leaf [{"summoner_name":"GIDEON"}] is correctly rejected).
// A malformed value (un-decodable) is treated as non-scalar — fail safe,
// never put questionable bytes on the wire.
func isLSDPScalar(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return false
	}
	return isScalarShape(decoded)
}

func isScalarShape(v any) bool {
	switch t := v.(type) {
	case nil, bool, float64, string, json.Number:
		return true
	case []any:
		for _, e := range t {
			if !isScalarShape(e) {
				return false
			}
		}
		return true
	default:
		// map[string]any (JSON object) and any other shape are forbidden.
		return false
	}
}

// mapCause maps Orion's provenance to the kit's. Both are audit-only
// (never load-bearing), so the mapping is a verbatim field copy.
func mapCause(c *protocol.Cause) *lproto.Cause {
	if c == nil {
		return nil
	}
	return &lproto.Cause{Source: c.Source, InputID: c.InputID}
}

// mapRole maps an Orion auth.Role to the kit protocol.Role used by the
// header-trust identity. The kit has no admin role; an admin writes
// everywhere, which is the operator privilege, so admin → operator.
// Anonymous / unknown roles return ok=false so the caller yields an
// Anonymous identity (kit treats it as auth failure).
func mapRole(r auth.Role) (lproto.Role, bool) {
	switch r {
	case auth.RoleViewer:
		return lproto.RoleViewer, true
	case auth.RoleOperator:
		return lproto.RoleOperator, true
	case auth.RoleService:
		return lproto.RoleService, true
	case auth.RoleAdmin:
		return lproto.RoleOperator, true
	default:
		return "", false
	}
}
