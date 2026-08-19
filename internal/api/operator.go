package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
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
// ACTIVE-ONLY (ADR 008 invariant): all three operate against the live
// antenna ONLY. A blueprint that is not part of the active scene is
// dormant — its entrypoints cannot be called and its awaits do not exist —
// answered 409 (call) / 410 (resolve) / empty (pending). This matches ADR
// Orion 008: only the active scene executes, so only its operator surface
// is live.
//
// ENGINE B (ORION-OPERATOR-RAIL-ENGINE-B #335, extended by
// ORION-OPERATOR-PREVIEW-ENGINE-B): all three routes now target
// bluehost.Host (isEngineBRouted) for BOTH the antenna (SlotOnAir) and the
// cockpit preview (SlotPreview, ?target=preview) — Show/PreviewSlot
// (Engine A) are structurally dead for these three routes in production:
// Show's roster has had no populator since POST /show/active-scene was
// retired (#15, #331), and PreviewSlot's only populator,
// POST /show/preview-active-scene, was retired the same way — Prism has
// pushed preview through POST /host/scene-intent's prepare-preview action
// (scene_intent.go) since #742, which lands in bluehost.Host's SlotPreview,
// not PreviewSlot. Routing target=preview through Engine A therefore read a
// slot nothing filled: every preview operator gesture 409/410'd
// (BLUEPRINT_NOT_ACTIVE / AWAIT_GONE) even with a scene genuinely prepared
// in preview. Only ?rule= (a stream-level rule selector) still resolves
// through Show (Engine A) — see isEngineBRouted's doc for why that one
// selector does not move.
//
// PreviewSlot/deps.Preview and POST /show/preview-active-scene are left in
// place by this change (not this work unit's call to retire — a possible
// dead-wire cleanup on the LSDP side, /show/preview.lsdp, is a separate,
// undecided chantier).
//
// ENGINE B PENDING (closed, blueruntime #344): getRuntimePending's Engine B
// leg now joins bluehost.Host's live armed-await registry
// (Host.PendingAwaitNames) against its declared metadata
// (Host.DeclaredContracts' AwaitDecl) by await_name — engineBArmedAwaits,
// the same join cockpit.go's appendEngineBScene uses for the `awaits` facet
// of GET /cockpit/contracts. A declared-but-never-armed await is never
// iterated (only live names drive the join), so it never surfaces as a
// prompt; an armed name absent from the declared set — not producible from
// a single valid program, both read the same static config — is omitted
// and logged rather than served with fabricated value_type. See
// engineBArmedAwaits's doc.
//
// UNKNOWN TARGET (Prism#740, ORION-UNKNOWN-TARGET-CONTRACT): a ?target=
// value that is neither absent nor the literal "preview" used to fall open
// to the antenna — harmless while preview read a dead Engine A slot, newly
// dangerous once preview became a real, populated one (a malformed client
// query string would fire on the live antenna instead of erroring or
// landing on preview). resolveTargetKind rejects it with 400
// UNKNOWN_TARGET instead, and is the SINGLE place every ?target=-reading
// route (these three, plus GET /cockpit/contracts) validates the raw query
// value — see that function's doc for why it replaced two independently-
// written, silently-diverging checks. `target` absent is unaffected: it is
// still, and only ever was, the antenna.
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

// operatorTarget resolves which scene the operator surface acts on, mode-aware
// (preview/antenne split): “?target=preview“ drives the PREVIEW slot's live
// clone (the cockpit preview runs there, not the global show), every other
// value drives the global show's active scene (the antenne). nil when the
// requested target has no live scene — the caller answers the usual dormant
// code (409/410/empty). This is the ONLY place the operator routes branch on
// target: a preview gesture drives the preview clone, a live gesture the
// antenne, never crossed (a preview switch / call no longer touches the live
// antenne, ADR 008 active-only preserved per side).
func operatorTarget(deps PublicDeps, r *http.Request) *runtime.Scene {
	if r.URL.Query().Get("target") == "preview" {
		if deps.Preview == nil {
			return nil
		}
		return deps.Preview.Current()
	}
	return deps.Show.Active()
}

// resolveOperatorScene picks the scene the operator routes act on. Without a
// selector it is the active/preview scene (operatorTarget). With ?rule={rule_id}
// (ADR 009 Amendment 1, #286) it is the promoted stream-level rule with that id
// — addressable now that #285 stamps the id on the cockpit contract.
//
// ?rule and ?target=preview are MUTUALLY EXCLUSIVE (400 SELECTOR_CONFLICT):
// "preview-firing a stream rule" is meaningless today, so a silent precedence
// is refused (Vigil, Amendment review). An unknown/demoted rule is 409
// RULE_NOT_ACTIVE (mirror of BLUEPRINT_NOT_ACTIVE) — the id named no currently
// promoted rule.
//
// Returns (scene, ok). ok=false means it has already written the error and the
// handler must return. ok=true with a nil scene is the non-rule path's normal
// "no live target" (no active scene / no preview) — the caller answers its own
// per-route dormant code (409/410/empty), behaviour unchanged.
func resolveOperatorScene(w http.ResponseWriter, deps PublicDeps, r *http.Request) (*runtime.Scene, bool) {
	q := r.URL.Query()
	ruleID := q.Get("rule")
	if ruleID == "" {
		return operatorTarget(deps, r), true
	}
	if q.Get("target") == "preview" {
		writeOperatorError(w, http.StatusBadRequest, "SELECTOR_CONFLICT",
			"?rule and ?target=preview are mutually exclusive")
		return nil, false
	}
	scene := deps.Show.StreamRuleScene(ruleID)
	if scene == nil {
		writeOperatorError(w, http.StatusConflict, "RULE_NOT_ACTIVE",
			"no promoted stream-level rule by that id")
		return nil, false
	}
	return scene, true
}

// isEngineBRouted reports whether r has no stream-rule selector — the only
// case that decides Engine A vs Engine B for call/resolve/pending. Both the
// antenna (no ?target, or any value but "preview") and the cockpit preview
// (?target=preview) now route through Engine B (bluehost.Host) — see
// engineBSlot for which slot. Only ?rule={rule_id} stays on Engine A
// (runtime.Scene, resolveOperatorScene's Show.StreamRuleScene lookup):
// stream-level rules capacity is paused (#15/#331: HTTP surface + handlers
// removed with internal/store) and its successor — composing a rule INTO
// the program a flow serves — is tracked but not yet built (Orion#332 R6
// ledger + ZabCanvas durability). Engine B has no rule concept to route to
// today, so leaving ?rule= on Engine A's (structurally empty in prod)
// registry is the coherent choice: it degrades to a deterministic 409
// RULE_NOT_ACTIVE, exactly the "no caller regression" posture public.go's
// route-registration comment already documents, without inventing a new
// slot/state on the Engine B side.
//
// ?rule and ?target=preview remain mutually exclusive (resolveOperatorScene
// writes 400 SELECTOR_CONFLICT) — unchanged by this function's widened
// scope, since a non-empty ?rule always short-circuits here regardless of
// ?target.
func isEngineBRouted(r *http.Request) bool {
	return r.URL.Query().Get("rule") == ""
}

// targetKind is the two literals ?target= recognizes across every route that
// reads it — the vocabulary resolveTargetKind validates against. Recognizing
// a third literal is a decision for that function alone; nothing else in
// this package re-parses the raw query value.
type targetKind int

const (
	targetAntenna targetKind = iota
	targetPreview
)

// resolveTargetKind is the SINGLE place that interprets and validates the
// raw ?target= query value (Prism#740, ORION-UNKNOWN-TARGET-CONTRACT):
// absent means the antenna, the exact literal "preview" means the cockpit
// preview clone, and — this is the change — anything else is rejected with
// 400 UNKNOWN_TARGET rather than silently treated as the antenna.
//
// Before this function existed, two call sites each re-read
// r.URL.Query().Get("target") independently and disagreed only by omission:
// engineBSlot (below) fell open to the antenna on any unrecognized value,
// and getCockpitContracts (cockpit.go) had its own literal `== "preview"`
// check with the identical fall-open shape, hardcoding bluehost.SlotOnAir in
// its else branch — never calling engineBSlot at all. Both are pinned by
// git history as independently-written, not a refactor of one another. A
// third literal typed into either check would have silently diverged from
// the other. Now both call resolveTargetKind and neither re-parses the
// query string itself — the SELECTOR_CONFLICT/RULE_NOT_ACTIVE convention
// error family this mirrors, UNKNOWN_TARGET, is nameable exactly once.
//
// ok=false means the 400 is already written; the caller returns immediately
// (same contract as resolveOperatorScene). The kind returned alongside
// ok=false is targetAntenna and MUST NOT be used — Go has no null enum, this
// is the zero value, not a meaningful default.
//
// This function only recognizes the two kinds; it does not decide which
// engine or slot serves them — engineBSlot maps a validated kind to the
// bluehost.Host slot for the three Engine-B routes (call/resolve/pending);
// getCockpitContracts maps the same kind to its own two engines (its
// ?target=preview leg is still Engine A, deps.Preview — see that function's
// doc for why that is dead code in production today and deliberately out of
// this work unit's scope to re-route).
func resolveTargetKind(w http.ResponseWriter, r *http.Request) (targetKind, bool) {
	switch r.URL.Query().Get("target") {
	case "":
		return targetAntenna, true
	case "preview":
		return targetPreview, true
	default:
		writeOperatorError(w, http.StatusBadRequest, "UNKNOWN_TARGET",
			`target must be absent or "preview"`)
		return targetAntenna, false
	}
}

// engineBSlot maps an already-validated targetKind (resolveTargetKind) to
// the bluehost.Host slot it addresses: preview drives the cockpit preview
// clone (bluehost.SlotPreview, populated by POST /host/scene-intent's
// prepare-preview action — scene_intent.go), antenna drives the live show
// (bluehost.SlotOnAir, populated by its take-on-air action). Mirrors
// operatorTarget's Engine A branch one-for-one, against the other engine.
// Every caller of this function has already rejected an unrecognized target
// via resolveTargetKind — this is the ONLY place that maps a valid kind to a
// slot, so a preview gesture can never reach SlotOnAir or vice versa
// (isolation invariant the bail requires: preparing a scene in preview must
// never fire on the antenna).
func engineBSlot(kind targetKind) bluehost.Slot {
	if kind == targetPreview {
		return bluehost.SlotPreview
	}
	return bluehost.SlotOnAir
}

// engineBHost is the Engine B instance host for the antenna, or nil when
// the stateless-cutover surface isn't provisioned (SceneIntent nil) — same
// degrade-gracefully posture as every other nil-dependency seam in this
// package (Preview, LSDPHandler, ...).
func engineBHost(deps PublicDeps) *bluehost.Host {
	if deps.SceneIntent == nil {
		return nil
	}
	return deps.SceneIntent.Host
}

// engineBArmedAwaits joins bluehost.Host's live armed-await registry
// (Host.PendingAwaitNames, wrapping blueruntime.Runtime.PendingAwaitNames
// #344) against its declared metadata (Host.DeclaredContracts' AwaitDecl)
// for slot, by await_name — the single join both getRuntimePending and
// cockpit.go's appendEngineBScene serve their awaits facet from. Every item
// is addressed under defaultBlueprintToken (Engine B hosts no named
// blueprint dimension — see postOperatorCallEngineB's doc). Nil when
// nothing is armed.
//
// A declared-but-unarmed await is never iterated — only armed names drive
// the loop — which IS the live-vs-declared distinction this join exists to
// draw (an idle await must never read as a live prompt, same posture as
// Engine A's listPendingAwaits "membership ⇒ armed"). An armed name absent
// from the declared set has no known producer: DeclaredContracts and the
// live registry both read the same static await_name off the same loaded
// program's await-value node config, so they cannot name-diverge from one
// program alone. Handled defensively anyway, never assumed away: neither
// runtime.PendingAwait nor cockpitAwait has room for a fabricated
// value_type (never invent metadata), so that name is omitted and logged
// — nameable, not hidden — rather than served or silently dropped.
func engineBArmedAwaits(host *bluehost.Host, slot bluehost.Slot, logger *slog.Logger) []runtime.PendingAwait {
	armed := host.PendingAwaitNames(slot)
	if len(armed) == 0 {
		return nil
	}
	_, declaredAwaits := host.DeclaredContracts(slot)
	declByName := make(map[string]bluehost.AwaitDecl, len(declaredAwaits))
	for _, a := range declaredAwaits {
		declByName[a.AwaitName] = a
	}
	out := make([]runtime.PendingAwait, 0, len(armed))
	for _, name := range armed {
		decl, ok := declByName[name]
		if !ok {
			if logger != nil {
				logger.Warn("engine b: armed await has no declared metadata, omitted from pending list",
					"await_name", name)
			}
			continue
		}
		out = append(out, runtime.PendingAwait{
			BlueprintKey: defaultBlueprintToken,
			AwaitName:    decl.AwaitName,
			ValueType:    decl.ValueType,
			UI:           decl.UI,
		})
	}
	return out
}

// decodeOperatorPayload decodes a raw JSON call/resolve body into the `any`
// bluehost.Host's portable ABI expects (Engine A passes json.RawMessage
// straight through instead — FireOnCall/ResolveAwait). An absent body
// decodes to a nil payload, preserving Engine A's null-payload convention.
func decodeOperatorPayload(w http.ResponseWriter, raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var payload any
	if json.Unmarshal(raw, &payload) != nil {
		writeOperatorError(w, http.StatusBadRequest, "BAD_BODY", "malformed JSON payload")
		return nil, false
	}
	return payload, true
}

// isBlueRuntimeCode reports whether err is a *blueruntime.Error carrying
// code — the portable ABI's typed failure envelope (blue.runtime.error.v1).
func isBlueRuntimeCode(err error, code string) bool {
	var berr *blueruntime.Error
	return errors.As(err, &berr) && berr.Code == code
}

// postOperatorCallEngineB serves the Engine B leg of POST /operator/call
// against bluehost.Host's slot (SlotOnAir for the antenna, SlotPreview for
// ?target=preview — see engineBSlot) — the stateless-cutover analogue of
// Engine A's active-only routing (ADR 008), now covering both slots.
//
// blueprint_id IS ACCEPTED BUT NOT ROUTED ON. Verified against the primary
// source (Blue/src/blue_engine/program/compiler.py:1000-1011, the compiler
// that actually produces a scene's blue.program.v1): an on-call entrypoint's
// wire id (`call_id`) is `config.entrypoint` (an authored value on the
// node), falling back to the raw node id — NEVER blueprint-key-prefixed —
// and the compiler enforces call_id uniqueness PROGRAM-WIDE, hard-failing
// the compile (PROGRAM_ENTRYPOINT_INVALID) on a collision. So a composed
// program's on-call ids are flat and globally unique by construction; there
// is no blueprint-scoped namespace for a `blueprint_id` path segment to
// reconstruct, and Engine A's `<blueprint_key>/<entry_id>` composite key
// (internal/runtime/exec.go entryKey) has no Engine B equivalent to route
// through. Prism sends a real, non-default blueprint_id on every call
// (cockpit-api.ts, real bindings) — gating on it here would 409 every real
// operator button while looking "fixed". HasTrigger (below) already fails
// closed on an unmatched entrypoint_id alone, so accepting-not-routing
// blueprint_id costs no safety: an entrypoint absent from the served
// program is still rejected, whatever blueprint_id rode along with it.
func postOperatorCallEngineB(w http.ResponseWriter, r *http.Request, deps PublicDeps, slot bluehost.Slot, entrypointID string) {
	host := engineBHost(deps)
	live := host != nil && host.Digest(slot) != ""
	if !live {
		writeOperatorError(w, http.StatusConflict, "BLUEPRINT_NOT_ACTIVE",
			"blueprint is not part of the active scene")
		return
	}
	if !host.HasTrigger(slot, entrypointID) {
		writeOperatorError(w, http.StatusConflict, "ENTRYPOINT_UNKNOWN",
			"no on-call entrypoint by that id on the active blueprint")
		return
	}

	var body operatorCallBody
	if !readOperatorBody(w, r, &body) {
		return
	}
	payload, ok := decodeOperatorPayload(w, body.Payload)
	if !ok {
		return
	}
	result, err := host.Call(slot, entrypointID, payload)
	if err != nil {
		writeOperatorError(w, http.StatusInternalServerError, "INTERNAL", "call failed")
		return
	}
	// The production stateless path owns a bridge for every scene slot. A
	// direct forward is required here: the runtime result contains the LSML
	// mutations caused by LEC, while the periodic bridge tick may have no
	// output to repeat. Keep old no-wire fixtures compatible, but never report
	// success when the production bridge exists and cannot project the result.
	if deps.SceneIntent != nil && deps.SceneIntent.Bridges != nil {
		if err := deps.SceneIntent.Bridges.Forward(slot, bluewire.StepResult{
			RuntimeSequence: result.RuntimeSequence,
			Outputs:         result.Outputs,
		}); err != nil {
			writeOperatorError(w, http.StatusServiceUnavailable, "PROJECTION_FAILED", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "fired"})
}

// postOperatorResolveEngineB serves the Engine B leg of POST
// /operator/resolve against bluehost.Host's slot (SlotOnAir for the
// antenna, SlotPreview for ?target=preview — see engineBSlot). blueprint_id
// is accepted but not routed on — same rationale as postOperatorCallEngineB
// (await names are compiler-declared per-node config, never
// blueprint-key-namespaced either). Unlike Call, blueruntime.Runtime.Resolve
// already fails closed on an unparked/unknown await name on its own
// (runtime.go: "Resolving an unparked/already-resolved name is reported,
// not silently dropped" — EVENT_MALFORMED) — no pre-check equivalent to
// HasTrigger is needed here.
func postOperatorResolveEngineB(w http.ResponseWriter, deps PublicDeps, slot bluehost.Slot, awaitName string, rawValue json.RawMessage) {
	host := engineBHost(deps)
	live := host != nil && host.Digest(slot) != ""
	if !live {
		writeOperatorError(w, http.StatusGone, "AWAIT_GONE",
			"no live await for this blueprint (inactive or invalidated)")
		return
	}
	value, ok := decodeOperatorPayload(w, rawValue)
	if !ok {
		return
	}
	_, err := host.Resolve(slot, awaitName, value)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
	case isBlueRuntimeCode(err, "AWAIT_TYPE_MISMATCH"):
		writeOperatorError(w, http.StatusUnprocessableEntity, "VALUE_TYPE_MISMATCH",
			"value does not match the await's value_type")
	case isBlueRuntimeCode(err, "EVENT_MALFORMED"):
		writeOperatorError(w, http.StatusGone, "AWAIT_GONE",
			"no live await by that name (already resolved or invalidated)")
	default:
		writeOperatorError(w, http.StatusInternalServerError, "INTERNAL", "resolve failed")
	}
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

		if isEngineBRouted(r) {
			kind, ok := resolveTargetKind(w, r)
			if !ok {
				return
			}
			postOperatorCallEngineB(w, r, deps, engineBSlot(kind), entrypointID)
			return
		}

		active, ok := resolveOperatorScene(w, deps, r)
		if !ok {
			return
		}
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
// Lists the live operator awaits of the target scene's blueprint. An
// inactive blueprint has no awaits (empty list — ADR 008 active-only).
//
// Engine B routed (no ?rule=, both antenna and ?target=preview): joins the
// live armed registry against the declared metadata (engineBArmedAwaits,
// see the file header's "ENGINE B PENDING" note and that function's doc for
// the join and its one accepted gap). An unhosted slot (nil host or empty
// digest) answers the same empty list as no awaits armed — no separate
// dormant branch needed, PendingAwaitNames already reports nil on an
// unloaded slot. blueprint_id plays no role on this leg, matching
// call/resolve's "accepted but not routed on" stance.
//
// ?rule={rule_id}: unchanged, resolves through Engine A's promoted-rule
// registry exactly as before this change.
func getRuntimePending(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		if isEngineBRouted(r) {
			kind, ok := resolveTargetKind(w, r)
			if !ok {
				return
			}
			pending := []runtime.PendingAwait{}
			if host := engineBHost(deps); host != nil {
				if joined := engineBArmedAwaits(host, engineBSlot(kind), deps.Logger); joined != nil {
					pending = joined
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"pending": pending})
			return
		}

		blueprintID := resolveBlueprintKey(r.PathValue("blueprint_id"))
		active, ok := resolveOperatorScene(w, deps, r)
		if !ok {
			return
		}
		pending := []runtime.PendingAwait{}
		if active != nil && active.HostsBlueprint(blueprintID) {
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

		if isEngineBRouted(r) {
			kind, ok := resolveTargetKind(w, r)
			if !ok {
				return
			}
			postOperatorResolveEngineB(w, deps, engineBSlot(kind), awaitName, body.Value)
			return
		}

		active, ok := resolveOperatorScene(w, deps, r)
		if !ok {
			return
		}
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
