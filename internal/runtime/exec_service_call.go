package runtime

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/ZabLaboratory/Orion/internal/effects"
)

// OpServiceCall is the runtime op `core.service.call@1` lowers to (ADR
// Blue 002). It is the curated write-egress effect: registry-gated, path
// built from the baked route, token scoped to the route's token_paths,
// caller Authorization NEVER forwarded.
const OpServiceCall = "service.call"

// bakedRouteConfigKey is the reserved config slot the COMPILER stamps the
// resolved curated route into (compiler/egress.go). The `__` prefix is
// unauthored by construction, so the runtime trusts it as curated data.
const bakedRouteConfigKey = "__route"

// bakedRoute is the curated route the compiler baked into the node — the
// authored graph carries only (service, route_id); host/path/method/scope
// all come from HERE, never from the graph.
type bakedRoute struct {
	Service      string   `json:"service"`
	RouteID      string   `json:"route_id"`
	Method       string   `json:"method"`
	PathTemplate string   `json:"path_template"`
	Params       []string `json:"params"`
	TokenPaths   []string `json:"token_paths"`
}

// execServiceCall is the `service.call` op. DATA inputs: `params` (json
// map {template_param: string}) and `payload` (json). Config carries the
// compiler-baked `__route`. Outputs bind `status`/`ok`/`body` on `then`,
// `error` on `error` — the same exec-effect continuation shape as
// `http.request`.
func execServiceCall(s *Scene, t *execTask, node *ExecNode, inPort string) execOpOutcome {
	if inPort == effectCompletePort {
		return finishEffect(s, t, node, func(env map[string]json.RawMessage, value json.RawMessage) {
			bindServiceCallResult(node.ID, env, value)
		})
	}

	route, ok := bakedRouteOf(node)
	if !ok {
		// The compiler ALWAYS bakes `__route` on a valid push (an undeclared
		// route is rejected at compile, EGRESS_ROUTE_NOT_DECLARED). A missing
		// bake on air is a structural backstop — fail to `error`, never the
		// world, never a guess.
		return serviceCallError(s, node, "EGRESS_ROUTE_NOT_BAKED")
	}

	values := s.pullServiceParams(t, node)
	payload, _ := s.pullData(t, node, "payload")
	path, err := effects.BuildPath(route.PathTemplate, route.Params, values)
	if err != nil {
		return serviceCallError(s, node, "EGRESS_PARAM_MISSING: "+err.Error())
	}

	timeout := s.effectTimeout(t, node)
	e := s.effects
	key := s.nextWakeKey()
	run := func(ctx context.Context) effects.Result {
		if e == nil || e.ServiceCall == nil {
			return effects.Result{Err: "SERVICE_CALL_UNCONFIGURED"}
		}
		res, err := e.ServiceCall.Call(ctx, route.Method, path, route.TokenPaths, payload)
		if err != nil {
			return effects.Result{Err: "SERVICE_CALL_FAILED: " + err.Error()}
		}
		out, err := json.Marshal(res)
		if err != nil {
			return effects.Result{Err: "SERVICE_CALL_ENCODE: " + err.Error()}
		}
		return effects.Result{Value: out}
	}
	return s.effectOutcome(node, key, run, timeout)
}

// pullServiceParams reads the `params` json input into a {name: string}
// map for template construction. A scalar value is stringified; a missing
// / non-object input yields an empty map (the BuildPath backstop then
// errors on any required-but-absent param). Boolean/number params are
// rendered in their canonical JSON form.
func (s *Scene) pullServiceParams(t *execTask, node *ExecNode) map[string]string {
	out := map[string]string{}
	raw, ok := s.pullData(t, node, "params")
	if !ok {
		return out
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for k, v := range m {
		var str string
		if err := json.Unmarshal(v, &str); err == nil {
			out[k] = str
			continue
		}
		// Non-string scalar (number/bool): use its raw JSON text.
		out[k] = string(v)
	}
	return out
}

// bakedRouteOf reads the compiler-baked curated route off the node config.
func bakedRouteOf(node *ExecNode) (bakedRoute, bool) {
	raw, ok := node.Config[bakedRouteConfigKey]
	if !ok {
		return bakedRoute{}, false
	}
	var r bakedRoute
	if err := json.Unmarshal(raw, &r); err != nil || r.PathTemplate == "" || r.Method == "" {
		return bakedRoute{}, false
	}
	return r, true
}

// bindServiceCallResult maps a ServiceCallResult onto the node's output
// pins (status/ok/body). Shared by the live finish and the validation
// finish so the two can never drift.
func bindServiceCallResult(nodeID string, env map[string]json.RawMessage, value json.RawMessage) {
	var out effects.ServiceCallResult
	if err := json.Unmarshal(value, &out); err != nil {
		return
	}
	env[nodeID+".status"] = json.RawMessage(strconv.Itoa(out.Status))
	ok := "false"
	if out.Status >= 200 && out.Status < 300 {
		ok = "true"
	}
	env[nodeID+".ok"] = json.RawMessage(ok)
	if len(out.Body) > 0 {
		env[nodeID+".body"] = out.Body
	} else {
		env[nodeID+".body"] = json.RawMessage("null")
	}
}

// serviceCallError parks nothing — it routes the node straight to its
// `error` continuation with the given reason (structural failures that
// never touch the world).
func serviceCallError(s *Scene, node *ExecNode, reason string) execOpOutcome {
	s.logger.Warn("service.call refused", "node", node.ID, "reason", reason)
	key := s.nextWakeKey()
	return execOpOutcome{
		park:    true,
		parkKey: key,
		resume:  ExecTarget{Node: node.ID, Port: effectCompletePort},
		start: func() {
			s.resumeParkedWith(key, effectEnv(node.ID, effects.Result{Err: reason}))
		},
	}
}
