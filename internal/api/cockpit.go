package api

import (
	"net/http"

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
type cockpitParam struct {
	runtime.ContractParam
	Scope string `json:"scope"`
}

type cockpitTrigger struct {
	runtime.ContractTrigger
	Scope string `json:"scope"`
}

type cockpitAwait struct {
	runtime.ContractAwait
	Scope string `json:"scope"`
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

		// Active scene → scope `scene` (vanishes on a flip).
		if active := deps.Show.Active(); active != nil {
			appendScene(&out, active, scopeScene)
		}
		// Promoted stream-level rules → scope `stream` (permanent).
		for _, rule := range deps.Show.StreamRuleScenes() {
			appendScene(&out, rule, scopeStream)
		}

		writeJSON(w, http.StatusOK, out)
	})
}

// appendScene derives one scene's contract and appends its facet items to the
// aggregate, stamped with scope. A scene whose loop is gone (nil contracts)
// contributes nothing.
//
// The runtime carries the DEFAULT (legacy single / blueprint-free) blueprint
// under the empty scene-local key "". An empty `blueprint_id` cannot be
// addressed by the operator routes — Go's ServeMux never matches an empty path
// segment, so the cockpit would build an unroutable `POST /operator/call//…`
// (404, e2e #152 gap). So the contract ANNOUNCES the empty key as the
// addressing token `_` (defaultBlueprintToken); the operator routes decode it
// back to "" (resolveBlueprintKey). This is an API-boundary alias only — the
// runtime keying is untouched, the round-trip is exact.
func appendScene(out *cockpitContracts, scene *runtime.Scene, scope string) {
	sc := scene.OperatorContracts()
	if sc == nil {
		return
	}
	for _, p := range sc.Params {
		out.Params = append(out.Params, cockpitParam{ContractParam: p, Scope: scope})
	}
	for _, t := range sc.Triggers {
		t.BlueprintKey = addressBlueprintKey(t.BlueprintKey)
		out.Triggers = append(out.Triggers, cockpitTrigger{ContractTrigger: t, Scope: scope})
	}
	for _, a := range sc.Awaits {
		a.BlueprintKey = addressBlueprintKey(a.BlueprintKey)
		out.Awaits = append(out.Awaits, cockpitAwait{ContractAwait: a, Scope: scope})
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
