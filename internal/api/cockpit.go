package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Cockpit contract surface (Orion #210, Blue ADR 008 §3.5). ONE read the
// cockpit issues to render the live operator UI of every active Blue rule. The
// contract is DERIVED by introspection (never stored): for the show's active
// scene and every promoted stream-level rule, it aggregates three facets —
// params (interface ports + UI, #165), triggers (on-call entrypoints, #209),
// awaits (pending operator awaits, #209 registry) — each tagged with its scope.
//
// SCOPE (ADR 009 + §3.5): the active scene's contracts are `scene`-scoped —
// they vanish on a scene flip. A promoted stream-level rule's contracts are
// `stream`-scoped — permanent, they survive the flip. The scope is the ROLE
// the hosting scene plays, stamped here, not a property of the blueprint.
//
// AUTHZ: GET, read-only — but it reveals the structure of every active rule
// (the live operator prompts), so it is operator-or-admin gated exactly like
// the other operator routes (#209, requireOperator). No operator value, no
// secret, ever appears in the output: only declared shape (paths, names,
// types, UI hints).
//
// STREAM: Orion runs a single live show. The `stream_id` query param is the
// cockpit's addressing of that show; it is accepted (and required, per the
// frozen contract) but Orion has no multi-stream partition — the current show
// IS the stream. An absent stream_id is a 400; any value resolves to the show.
//
// ENGINE B ANTENNA (ORION-OPERATOR-RAIL-ENGINE-B, #335): the antenna's
// scene-scope facet now derives from bluehost.Host's on-air instance
// (appendEngineBScene), not Show.Active() — Show's roster is structurally
// empty in production since #331. ?target=preview is unchanged (still
// Engine A, operatorTarget). The antenna's awaits facet now joins
// blueruntime's live armed-await registry (Host.PendingAwaitNames, #344)
// against bluehost's declared metadata (AwaitDecl) — see appendEngineBScene's
// doc for the join and its one accepted gap (an armed await absent from the
// declared set — never producible via the compiler on a single program, see
// that doc — is omitted, not fabricated).
//
// UNKNOWN TARGET (Prism#740, ORION-UNKNOWN-TARGET-CONTRACT): ?target= is now
// validated by operator.go's resolveTargetKind before either branch below
// runs — an unrecognized value is 400 UNKNOWN_TARGET, not a silent fall
// through to the antenna branch. See getCockpitContracts's inline comment
// for why the preview leg stays on Engine A regardless.

// cockpitContractItem<T> is a facet item wrapped with its scope. Because Go
// has no generics-in-JSON-shape ergonomics here, each facet has its own typed
// wrapper carrying the same `scope` field.

// scopeScene / scopeStream are the two contract scopes (ADR 008 §3.5).
const (
	scopeScene  = "scene"
	scopeStream = "stream"
)

// cockpitParam / cockpitTrigger / cockpitAwait are the scope-stamped facet
// items the route emits. They embed the runtime's role-agnostic facet shape
// and add `scope`.
//
// `rule_id` (ADR 009 Amendment 1, #285) is the stable id of the promoted
// stream-level rule an item belongs to — the key `streamRules` holds
// (scene_id or blueprint_id), which #286 targets via `?rule={rule_id}`.
// It is additive and `omitempty`: only stream-scope items carry it, so
// scene-scope items are byte-for-byte unchanged (frozen Conduit contract
// #213 preserved).
type cockpitParam struct {
	runtime.ContractParam
	Scope  string `json:"scope"`
	RuleID string `json:"rule_id,omitempty"`
}

type cockpitTrigger struct {
	runtime.ContractTrigger
	Scope  string `json:"scope"`
	RuleID string `json:"rule_id,omitempty"`
}

type cockpitAwait struct {
	runtime.ContractAwait
	Scope  string `json:"scope"`
	RuleID string `json:"rule_id,omitempty"`
}

// cockpitContracts is the GET /cockpit/contracts response body. The three
// facets are flat, scope-stamped lists — the cockpit reads each into its own
// renderer (controls / buttons / prompts). Order is deterministic: scene-scope
// items first (active scene), then stream-scope items (rules sorted by id).
type cockpitContracts struct {
	StreamID string           `json:"stream_id"`
	Params   []cockpitParam   `json:"params"`
	Triggers []cockpitTrigger `json:"triggers"`
	Awaits   []cockpitAwait   `json:"awaits"`
}

// getCockpitContracts handles GET /api/v1/cockpit/contracts?stream_id=X.
// Aggregates the derived operator contract over the active scene (scope
// `scene`) and every promoted stream-level rule (scope `stream`).
func getCockpitContracts(deps PublicDeps) http.HandlerFunc {
	return requireOperator(func(w http.ResponseWriter, r *http.Request) {
		streamID := r.URL.Query().Get("stream_id")
		if streamID == "" {
			writeOperatorError(w, http.StatusBadRequest, "STREAM_ID_REQUIRED",
				"stream_id query parameter is required")
			return
		}

		out := cockpitContracts{
			StreamID: streamID,
			Params:   []cockpitParam{},
			Triggers: []cockpitTrigger{},
			Awaits:   []cockpitAwait{},
		}

		// Scene-scope contract → the operator surface the rail drives. Mode-aware
		// (preview/antenne split), validated by the same single point every
		// ?target=-reading route shares (resolveTargetKind, operator.go —
		// Prism#740, ORION-UNKNOWN-TARGET-CONTRACT): an unrecognized value is now
		// 400 UNKNOWN_TARGET rather than silently falling open to the antenna.
		// ``target=preview`` derives it from the PREVIEW slot's live clone
		// (Engine A, unchanged — operatorTarget); ``target`` absent derives from
		// the ANTENNA's Engine B on-air instance (ORION-OPERATOR-RAIL-ENGINE-B,
		// #335) — Show.Active() has had no production populator since #331 and is
		// structurally empty. Vanishes on a flip/take of whichever side it reads.
		//
		// The preview leg here is STILL Engine A (deps.Preview), unlike
		// call/resolve/pending's engineBSlot — deliberately not re-routed by this
		// work unit (its only populator, POST /show/preview-active-scene, was
		// retired the same way Show's was, so this leg is dead in production
		// today; re-pointing it at Engine B's SlotPreview is a real fix but a
		// different, undecided chantier, not a side effect of hardening the
		// unknown-target reject).
		kind, ok := resolveTargetKind(w, r)
		if !ok {
			return
		}
		switch kind {
		case targetPreview:
			if active := operatorTarget(deps, r); active != nil {
				appendScene(&out, active, scopeScene, "")
			}
		case targetAntenna:
			if host := engineBHost(deps); host != nil {
				appendEngineBScene(&out, host, bluehost.SlotOnAir, deps.Logger)
			}
		}
		// Promoted stream-level rules live on the global Engine B RulePlane,
		// never in either scene program. Therefore the same stream facets are
		// appended for antenna and preview targets and survive every slot flip.
		if deps.StreamRules != nil && deps.StreamRules.Plane != nil {
			appendRulePlane(&out, deps.StreamRules.Plane, deps.Logger)
		} else {
			// Compatibility seam for isolated Engine A fixtures and callers that
			// intentionally construct PublicDeps without the production plane.
			for _, rule := range deps.Show.StreamRuleScenes() {
				appendScene(&out, rule, scopeStream, rule.ID())
			}
		}

		writeJSON(w, http.StatusOK, out)
	})
}

// appendRulePlane derives every global rule's Engine B operator contract.
// rule_id and blueprint_id are both the promoted blueprint id, preserving the
// frozen cockpit routing contract used by ?rule={rule_id}. Params are absent:
// a stream rule has no LSML render bundle or Canvas-owned scene interface.
func appendRulePlane(out *cockpitContracts, plane *bluehost.RulePlane, logger *slog.Logger) {
	for _, contract := range plane.Contracts() {
		for _, trigger := range contract.Triggers {
			out.Triggers = append(out.Triggers, cockpitTrigger{
				ContractTrigger: runtime.ContractTrigger{
					BlueprintKey: contract.RuleID,
					EntrypointID: trigger.CallID,
					State:        "armed",
					UI:           trigger.UI,
				},
				Scope:  scopeStream,
				RuleID: contract.RuleID,
			})
		}

		declared := make(map[string]bluehost.AwaitDecl, len(contract.Awaits))
		for _, await := range contract.Awaits {
			declared[await.AwaitName] = await
		}
		for _, name := range plane.PendingAwaitNames(contract.RuleID) {
			await, ok := declared[name]
			if !ok {
				if logger != nil {
					logger.Warn("stream rule armed await has no declared metadata, omitted", "rule_id", contract.RuleID, "await_name", name)
				}
				continue
			}
			out.Awaits = append(out.Awaits, cockpitAwait{
				ContractAwait: runtime.ContractAwait{
					BlueprintKey: contract.RuleID,
					AwaitName:    await.AwaitName,
					ValueType:    await.ValueType,
					State:        "armed",
					UI:           await.UI,
				},
				Scope:  scopeStream,
				RuleID: contract.RuleID,
			})
		}
	}
}

// appendScene derives one scene's contract and appends its facet items to the
// aggregate, stamped with scope and (for stream-scope) ruleID. A scene whose
// loop is gone (nil contracts) contributes nothing. ruleID is "" for the
// scene-scope active scene — `omitempty` then drops the field, keeping the
// scene item shape byte-for-byte stable (frozen contract #213).
//
// The runtime carries the DEFAULT (legacy single / blueprint-free) blueprint
// under the empty scene-local key "". An empty `blueprint_id` cannot be
// addressed by the operator routes — Go's ServeMux never matches an empty path
// segment, so the cockpit would build an unroutable `POST /operator/call//…`
// (404, e2e #152 gap). So the contract ANNOUNCES the empty key as the
// addressing token `_` (defaultBlueprintToken); the operator routes decode it
// back to "" (resolveBlueprintKey). This is an API-boundary alias only — the
// runtime keying is untouched, the round-trip is exact.
func appendScene(out *cockpitContracts, scene *runtime.Scene, scope, ruleID string) {
	sc := scene.OperatorContracts()
	if sc == nil {
		return
	}
	for _, p := range sc.Params {
		out.Params = append(out.Params, cockpitParam{ContractParam: p, Scope: scope, RuleID: ruleID})
	}
	for _, t := range sc.Triggers {
		t.BlueprintKey = addressBlueprintKey(t.BlueprintKey)
		out.Triggers = append(out.Triggers, cockpitTrigger{ContractTrigger: t, Scope: scope, RuleID: ruleID})
	}
	for _, a := range sc.Awaits {
		a.BlueprintKey = addressBlueprintKey(a.BlueprintKey)
		out.Awaits = append(out.Awaits, cockpitAwait{ContractAwait: a, Scope: scope, RuleID: ruleID})
	}
}

// addressBlueprintKey maps a runtime scene-local blueprint key to the HTTP
// addressing token the cockpit emits in `blueprint_id`. The empty (default)
// key becomes `_`; every other key passes through verbatim. Exact inverse of
// resolveBlueprintKey (operator.go).
func addressBlueprintKey(key string) string {
	if key == "" {
		return defaultBlueprintToken
	}
	return key
}

// appendEngineBScene derives the antenna's Engine B contract (scope `scene`)
// and appends it to out — the Engine B analogue of appendScene(active, ...)
// for the one instance bluehost.Host hosts per slot (ORION-OPERATOR-RAIL-
// ENGINE-B, #335). No-op when slot holds no instance.
//
// Params come from the LSML render-bundle's `operator_inputs` field (same
// generic, best-effort decode getOperatorInputs already uses for this same
// field — ZabCanvas's lsml_bundle schema for it is NOT a confirmed-matching
// contract against Orion's own compiler shape, so a field that doesn't
// decode just stays zero-valued, never an error).
//
// Triggers come from the loaded program's declared on-call entrypoints
// (bluehost.Host.DeclaredContracts), addressed under defaultBlueprintToken
// since Engine B hosts no named blueprint dimension today (see
// postOperatorCallEngineB's doc).
//
// Awaits are the cockpit's view of engineBArmedAwaits (operator.go) — the
// same live-registry × declared-metadata join getRuntimePending serves the
// polling route from, by await_name. See that function's doc for the join
// semantics and its one accepted, unproducible-from-a-single-program gap
// (an armed name absent from the declared set, omitted and logged rather
// than served with fabricated value_type).
func appendEngineBScene(out *cockpitContracts, host *bluehost.Host, slot bluehost.Slot, logger *slog.Logger) {
	if host.Digest(slot) == "" {
		return
	}
	for _, p := range bundleOperatorInputs(host.Bundle(slot)) {
		out.Params = append(out.Params, cockpitParam{ContractParam: p, Scope: scopeScene})
	}
	triggers, _ := host.DeclaredContracts(slot)
	for _, t := range triggers {
		out.Triggers = append(out.Triggers, cockpitTrigger{
			ContractTrigger: runtime.ContractTrigger{
				BlueprintKey: defaultBlueprintToken,
				EntrypointID: t.CallID,
				State:        "armed",
				UI:           t.UI,
			},
			Scope: scopeScene,
		})
	}

	for _, a := range engineBArmedAwaits(host, slot, logger) {
		out.Awaits = append(out.Awaits, cockpitAwait{
			ContractAwait: runtime.ContractAwait{
				BlueprintKey: a.BlueprintKey,
				AwaitName:    a.AwaitName,
				ValueType:    a.ValueType,
				State:        "armed",
				UI:           a.UI,
			},
			Scope: scopeScene,
		})
	}
}

// bundleOperatorInputs extracts an LSML render-bundle's `operator_inputs`
// field into the shared ContractParam shape. Best-effort, generic top-level
// key decode (mirrors getOperatorInputs, scenes_get.go) rather than a typed
// contract Orion owns end-to-end: nil/malformed bundle or an unrecognised
// field shape yields nil (never an error) rather than a broken response.
func bundleOperatorInputs(bundle []byte) []runtime.ContractParam {
	if len(bundle) == 0 {
		return nil
	}
	var envelope struct {
		OperatorInputs []runtime.ContractParam `json:"operator_inputs"`
	}
	if json.Unmarshal(bundle, &envelope) != nil {
		return nil
	}
	return envelope.OperatorInputs
}
