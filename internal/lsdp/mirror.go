package lsdp

import (
	"bytes"
	"encoding/json"
	"sync"

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
	// bound is the renderable leaf surface of the active scene's bundle.
	// When active() it is the PRIMARY wire gate: only bound leaves (and
	// their descendants) are emitted, so every compute intermediate
	// (object rows, clause descriptors, empty WHERE literals, scalar work
	// leaves) is dropped at the tap regardless of its JSON shape. When
	// NOT active (no bindings in the bundle) the gate is disabled and the
	// isLSDPScalar shape filter alone backstops the wire — never an
	// all-drop black screen.
	bound boundLeafSet

	// identMu guards lastIdentity/hasIdentity — the sceneMirror's own
	// best-effort memory of the most recent projection identity a Delta
	// carried, kept so a later Snapshot forward can report whether an
	// identity is known, and (as a regression check, see
	// observeSnapshotIdentityGap) confirm it is expected to ride the
	// outgoing Snapshot frame via the kit's own stamping. Never itself sent
	// on any wire; never load-bearing — the kit's *lserver.Scene carries its
	// own independent copy (lastMetadata) that actually gets stamped.
	identMu      sync.Mutex
	lastIdentity lproto.ProjectionMetadata
	hasIdentity  bool
}

const bootstrapStatePath = "__zab.scene.ready"

var _ runtime.SceneMirror = (*sceneMirror)(nil)

// Forward maps a reactive output message onto the kit scene.
func (m *sceneMirror) Forward(msg runtime.SubscriberMsg) {
	switch v := msg.(type) {
	case *protocol.Snapshot:
		m.scene.SetVersion(v.SceneVersion)
		m.observeSnapshotIdentityGap()
		if len(v.State) == 0 {
			// lumencast-go rejects an empty patch set. Seed every authored
			// binding with neutral nulls when defaults are absent, so Solar
			// mounts the real tree without fabricating match data. A marker is
			// retained only for a bundle with no bindings at all.
			state := m.bound.bootstrapState()
			if len(state) == 0 {
				state = map[string]any{bootstrapStatePath: true}
			}
			_ = m.scene.Set(state)
			return
		}
		patches := make(map[string]any, len(v.State))
		for path, val := range v.State {
			if !m.wireLegal(path, val) {
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
			if !m.wireLegal(p.Path, p.Value) {
				continue
			}
			patches[p.Path] = p.Value
		}
		if len(patches) == 0 {
			// Every patch in this delta was a non-scalar intermediate.
			// Nothing wire-legal to emit; Emit rejects empty maps anyway.
			return
		}
		metadata := mapProjectionMetadata(v)
		m.recordIdentity(metadata)
		_ = m.scene.EmitWithCauseAndMetadata(
			patches,
			mapCause(v.Cause),
			metadata,
		)
	case *protocol.SceneChanged:
		// The scene switch is driven authoritatively from the Show via
		// Wire.SetActive (kit Server.SetActive migrates live subs with
		// its own scene_changed + snapshot). Nothing to do per-scene.
	}
}

// recordIdentity remembers the projection identity of the most recent Delta
// this mirror forwarded — metadata may be nil (a Delta with no projection
// fields at all, e.g. a legacy-engine-driven scene that never had one), in
// which case any PRIOR known identity is deliberately kept, not cleared: a
// later Snapshot on the SAME scene should still report the last real
// identity it dropped, not "none" just because the most recent Delta
// happened to carry no metadata.
func (m *sceneMirror) recordIdentity(metadata *lproto.ProjectionMetadata) {
	if metadata == nil {
		return
	}
	m.identMu.Lock()
	m.lastIdentity = *metadata
	m.hasIdentity = true
	m.identMu.Unlock()
}

// observeSnapshotIdentityGap makes explicit, at every Snapshot forward,
// whether a known projection identity exists for this scene, and whether
// the upcoming Snapshot frame (m.scene.Set, below in Forward) is expected to
// carry it.
//
// Before lumencast-go 0c7cfc6 (#22), protocol.Snapshot (the kit's wire type)
// had no metadata field at all — a known identity was unconditionally
// dropped on every Snapshot forward, and this function counted that as a
// real, unavoidable loss (Warn + metric). Since 0c7cfc6, m.scene
// (*lserver.Scene) remembers the metadata of the most recent
// EmitWithCauseAndMetadata call as its own lastMetadata and stamps it onto
// every Snapshot it later constructs. recordIdentity (below) updates
// m.lastIdentity/m.hasIdentity on that exact same call, against that exact
// same *lserver.Scene — so known==true here structurally guarantees the
// frame the kit is about to build for this scene will carry the identity.
// This is no longer a gap: log at Info for visibility, and do NOT count it —
// counting it would be a permanent false positive (a metric that cries wolf
// after the underlying bug is fixed is worse than no metric).
// SnapshotIdentityGap is kept wired (see wire.go's snapshotMetrics doc) as a
// regression guard, not deleted — it would need a NEW divergence between
// this bookkeeping and the kit's own state to ever have something real to
// count again, which nothing in this package currently causes.
//
// The !known branch is unaffected by the bump: the kit legitimately omits
// the metadata fields when nothing was ever learned for this scene — that
// is not a loss, only Info-logged, never counted. A nil wire logger/metrics
// sink makes this a no-op either way, matching every other optional sink
// here.
func (m *sceneMirror) observeSnapshotIdentityGap() {
	m.identMu.Lock()
	identity, known := m.lastIdentity, m.hasIdentity
	m.identMu.Unlock()

	if !known {
		if m.wire.logger != nil {
			m.wire.logger.Info("lsdp snapshot reseed: no projection identity known yet", "scene_id", m.sceneID)
		}
		return
	}
	if m.wire.logger != nil {
		m.wire.logger.Info("lsdp snapshot reseed carries known projection identity (lumencast-go stamps Scene.lastMetadata onto the Snapshot frame)",
			"scene_id", m.sceneID,
			"target", identity.Target,
			"scene_digest", identity.SceneDigest,
			"runtime_instance_id", identity.RuntimeInstanceID,
			"render_revision", identity.RenderRevision,
			"correlation_id", identity.CorrelationID,
		)
	}
}

// mapProjectionMetadata preserves Orion's additive projection identity while
// crossing into the canonical Lumencast server API. A delta without any
// projection fields stays on the legacy call shape so the 1.0/1.1 wire for
// existing callers remains byte-compatible.
func mapProjectionMetadata(v *protocol.Delta) *lproto.ProjectionMetadata {
	if v == nil || (v.SchemaVersion == "" &&
		v.SceneDigest == "" &&
		v.RuntimeInstanceID == "" &&
		v.Target == "" &&
		v.RenderRevision == "" &&
		v.CorrelationID == "") {
		return nil
	}
	return &lproto.ProjectionMetadata{
		SchemaVersion:     v.SchemaVersion,
		SceneDigest:       v.SceneDigest,
		RuntimeInstanceID: v.RuntimeInstanceID,
		Target:            v.Target,
		RenderRevision:    v.RenderRevision,
		CorrelationID:     v.CorrelationID,
	}
}

// wireLegal is the single wire-emission decision for a leaf, combining
// the two filters in priority order:
//
//  1. The BOUND-LEAF surface gate (primary, when the bundle binds at
//     least one leaf): emit a leaf only if the active scene's layout
//     binds it — or binds an ancestor of it (so `repeat.items` children
//     `items.{i}.field` survive). Every `__vars..` compute intermediate
//     that no node binds (object rows, clause descriptors, the empty
//     WHERE literals `whereEmptyN=[]`, the scalar work leaves
//     `catA0`/`getScore0`) is dropped here regardless of shape — the
//     definitive hygiene contract that ends present and future leakage.
//
//  2. The §3.2.1 SHAPE filter (defense-in-depth, always): even a bound
//     leaf must be scalar / array-of-scalar to be wire-legal — a bound
//     leaf that somehow holds an object must never reach
//     @lumencast/protocol (which would reject the whole frame). When the
//     bound gate is DISABLED (no bindings — passthrough/operator-only
//     scene, or a bundle that was not threaded), this shape filter is
//     the SOLE gate, exactly as before this change (fail-open, no
//     black screen).
//
// The bespoke /show/stream wire (ADR 002) does not go through this tap
// and is untouched.
func (m *sceneMirror) wireLegal(path string, val json.RawMessage) bool {
	if m.bound.active() && !m.bound.renderable(path) {
		return false
	}
	return isLSDPScalar(val)
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
