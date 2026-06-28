package compiler

import (
	"context"
	"encoding/json"
	"sort"
)

// opServiceCall is the runtime op `core.service.call@1` lowers to
// (conformance table: "core.service.call@1" → Op "service.call"). Kept
// local to avoid importing the runtime package from the compiler; a
// conformance cross-check keeps the two in sync.
const opServiceCall = "service.call"

// bakedRouteConfigKey is the reserved config slot the compiler stamps the
// resolved curated route into. The `__` prefix is unauthored by
// construction (Blue port/config names never use it), so an authored
// graph can neither set nor read it — the runtime trusts it as curated
// data, the precedent being `__resolved_source` for core.source.read@1.
const bakedRouteConfigKey = "__route"

// egressRouteFetcher is the OPTIONAL fetcher capability that surfaces
// Blue's curated egress registry (ADR Blue 002 §3.2). The compiler
// type-asserts it on the Fetcher so the core interface is unchanged; a
// fetcher that doesn't implement it yields a nil registry — fail-closed.
type egressRouteFetcher interface {
	FetchEgressRoutes(ctx context.Context) (EgressRegistry, error)
}

// bakedRoute is what the compiler stamps into a service.call node's
// Config. It is the CURATED route — method/path_template/token_paths
// resolved from the registry, never authored data — so the runtime
// builds the path and scopes the token without trusting the graph.
type bakedRoute struct {
	Service      string   `json:"service"`
	RouteID      string   `json:"route_id"`
	Method       string   `json:"method"`
	PathTemplate string   `json:"path_template"`
	Params       []string `json:"params"`
	TokenPaths   []string `json:"token_paths"`
}

// resolveEgressRoutes gates every `core.service.call@1` exec node of a
// partitioned program against the curated egress registry:
//
//   - an undeclared (service, route_id) → EGRESS_ROUTE_NOT_DECLARED
//     (structural compile reject — push fails closed, never on air);
//   - a declared one → the resolved route is baked into the node's Config
//     under `__route` so the runtime constructs the path from the curated
//     path_template (escaped params) and mints the token with the route's
//     token_paths only. The authored graph carries ONLY (service,
//     route_id); it can never influence host/path/method/scope.
//
// Nodes are visited in sorted id order so diagnostics are deterministic.
func resolveEgressRoutes(prog *execProgram, egress EgressRegistry) []Diagnostic {
	if prog == nil || len(prog.Nodes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(prog.Nodes))
	for id, n := range prog.Nodes {
		if n != nil && n.Op == opServiceCall {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)

	var diags []Diagnostic
	for _, id := range ids {
		n := prog.Nodes[id]
		service := configStringValue(n.Config, "service")
		routeID := configStringValue(n.Config, "route_id")
		route, ok := egress[EgressRouteKey(service, routeID)]
		if !ok {
			diags = append(diags, Diagnostic{
				Code:     ErrEgressRouteNotDeclared,
				Severity: "error",
				Message: "core.service.call node " + id +
					" targets egress route (" + service + ", " + routeID +
					") which is not declared in the curated egress registry",
				Path: id,
			})
			continue
		}
		baked, err := json.Marshal(bakedRoute(route))
		if err != nil {
			diags = append(diags, Diagnostic{
				Code:     ErrEgressRouteNotDeclared,
				Severity: "error",
				Message:  "core.service.call node " + id + ": route marshal failed",
				Path:     id,
			})
			continue
		}
		if n.Config == nil {
			n.Config = map[string]json.RawMessage{}
		}
		n.Config[bakedRouteConfigKey] = baked
	}
	return diags
}

// configStringValue reads a JSON string config value, or "" when the key
// is absent or not a string.
func configStringValue(cfg map[string]json.RawMessage, key string) string {
	raw, ok := cfg[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
