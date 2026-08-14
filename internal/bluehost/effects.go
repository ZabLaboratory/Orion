package bluehost

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// EffectDeps is the shared host-side dependency bundle. NewEffectHandlers
// into Engine B's 4 opcodes of full right (ENGINE-B-PARITY-BLUE:
// core.http.request@1, core.http-request@1, core.db.query@1,
// core.service.call@1 — the opcodes
// walker.go dispatches through StartOptions.EffectHandlers, NOT the
// core.effect.invoke@1 async admission protocol). The Runner is used by
// the generic invocation adapter; the other fields are used by the direct
// handlers. cmd/orion/main.go creates this bundle once and passes the same
// instances to Engine A and the scene-intent bluehost surface.
type EffectDeps struct {
	Runner      *effects.Runner
	Egress      *effects.EgressPolicy
	DataSources map[string]effects.DataSource
	DB          *effects.DBQueryClient
	ServiceCall *effects.ServiceCallClient
	// ResolveServiceRoute is the authoritative registry bridge for Blue's
	// canonical __route reference. The scene-intent/API path passes this
	// dependency bundle unchanged into NewEffectHandlers; nil is deliberately
	// fail-closed rather than an invitation to trust authored route details.
	ResolveServiceRoute ServiceRouteResolver
	EgressBudget        *effects.StreamEgressLimiter
	EgressBudgetKey     string
}

// ServiceCallRoute is the host-resolved portion of a compiler-curated
// service.call route. Blue's portable ABI carries only the opaque
// (service, route_id) reference under __route; Orion resolves that reference
// against the same authoritative route registry used by its compiler before
// constructing the gateway request. A missing resolver or route fails closed.
type ServiceCallRoute struct {
	Service      string   `json:"service"`
	RouteID      string   `json:"route_id"`
	Method       string   `json:"method"`
	PathTemplate string   `json:"path_template"`
	Params       []string `json:"params"`
	TokenPaths   []string `json:"token_paths"`
}

// ServiceRouteResolver resolves a Blue-baked service/route identifier to the
// host transport details. The callback is intentionally optional: callers
// without an authoritative registry retain the fail-closed behavior instead
// of accepting authored method, path, or token scope data.
type ServiceRouteResolver func(service, routeID string) (ServiceCallRoute, bool)

// NewEffectHandlers builds the StartOptions.EffectHandlers table for one
// instance. mode gates the transport: blueruntime.Preview NEVER dials the
// network or the DB — CheckURL/DB.Query are not even reached, proven by
// construction, satisfying issue #358 §7's explicit preview criterion —
// while blueruntime.Execute dispatches for real through deps, the same
// policy (allowlist, token, response caps) Engine A's on-air scene uses.
//
// Engine A's PreviewSlot applies the same stateless policy to its private
// effect bundle, so both host implementations expose the same synthetic
// preview observation before transport admission.
func NewEffectHandlers(deps EffectDeps, mode blueruntime.Mode) map[string]blueruntime.EffectFunc {
	http := func(_, inputs map[string]any) (map[string]any, error) {
		if mode != blueruntime.Execute {
			return previewHTTPResult(), nil
		}
		return doHTTPRequest(context.Background(), deps.Egress, inputs)
	}
	db := func(config, inputs map[string]any) (map[string]any, error) {
		if mode != blueruntime.Execute {
			return previewDBResult(), nil
		}
		return doDBQuery(context.Background(), deps.DB, deps.DataSources, config, inputs)
	}
	serviceCall := func(config, inputs map[string]any) (map[string]any, error) {
		if mode != blueruntime.Execute {
			return previewServiceCallResult(), nil
		}
		return doServiceCall(context.Background(), deps.ServiceCall, deps.ResolveServiceRoute, deps.EgressBudget, deps.EgressBudgetKey, config, inputs)
	}
	return map[string]blueruntime.EffectFunc{
		"core.http.request@1": http,
		"core.http-request@1": http,
		"core.db.query@1":     db,
		"core.service.call@1": serviceCall,
	}
}

// previewHTTPResult is the construction-safe, no-transport preview
// response: fires `then` (nil error), never touches the network — the
// same "unwired seam still fires then" ethos walker.go's
// fireLocalSideEffect applies to animation.play/show.emit/overlay-app.set.
func previewHTTPResult() map[string]any {
	return map[string]any{
		"status":  json.Number("0"),
		"body":    nil,
		"headers": map[string]any{},
		"ok":      false,
		"preview": true,
	}
}

func previewDBResult() map[string]any {
	return map[string]any{
		"rows":       nil,
		"count":      json.Number("0"),
		"elapsed_ms": json.Number("0"),
		"preview":    true,
	}
}

func previewServiceCallResult() map[string]any {
	return map[string]any{
		"status":  json.Number("0"),
		"body":    nil,
		"ok":      false,
		"preview": true,
	}
}

// doHTTPRequest executes `core.http.request@1`/`core.http-request@1` for
// real — Engine A's execHTTPRequest (internal/runtime/exec_effects.go)
// contract, adapted from Scene/ExecNode pulls to the portable ABI's plain
// config/inputs maps: `url`/`method`/`query`/`headers`/`body`/`timeout_ms`
// data inputs in, `status`/`body`/`headers`/`ok` out. Every failure mode
// (bad egress policy, malformed URL, network error) is returned as a Go
// error, which walker.go's fireEffectOp maps onto the node's `error` pin —
// never a crash, never a silent `then` (same effect semantics as Engine A).
func doHTTPRequest(ctx context.Context, egress *effects.EgressPolicy, inputs map[string]any) (map[string]any, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeoutFrom(inputs["timeout_ms"]))
	defer cancel()
	response, err := executeHTTP(timeoutCtx, egress, httpTransportRequest{
		URL:     strOf(inputs["url"]),
		Method:  strOf(inputs["method"]),
		Query:   inputs["query"],
		Headers: inputs["headers"],
		Body:    bodyBytes(inputs["body"]),
	})
	if err != nil {
		return nil, err
	}
	values := response.values()
	values["ok"] = response.Status >= 200 && response.Status <= 299
	return values, nil
}

// doDBQuery executes `core.db.query@1` for real — Engine A's execDBQuery
// (topology A) contract: `datasource` (config) selects a declared
// effects.DataSource, `descriptor` (data input, the QueryMe
// QueryDescriptor) is forwarded verbatim to deps.DB.Query. Outputs:
// `rows`/`count`/`elapsed_ms`.
func doDBQuery(ctx context.Context, db *effects.DBQueryClient, dataSources map[string]effects.DataSource, config, inputs map[string]any) (map[string]any, error) {
	if db == nil {
		return nil, fmt.Errorf("DB_QUERY_UNCONFIGURED: no _query client")
	}
	name := strOf(config["datasource"])
	ds, declared := dataSources[name]
	if !declared {
		return nil, fmt.Errorf("DATASOURCE_NOT_DECLARED: %s", name)
	}
	descriptor, err := json.Marshal(inputs["descriptor"])
	if err != nil {
		descriptor = []byte(`{}`)
	}
	res, err := db.Query(ctx, ds, descriptor)
	if err != nil {
		return nil, fmt.Errorf("DB_QUERY_FAILED: %w", err)
	}
	var rows any
	if len(res.Rows) > 0 {
		_ = json.Unmarshal(res.Rows, &rows)
	}
	return map[string]any{
		"rows":       rows,
		"count":      json.Number(strconv.Itoa(res.Count)),
		"elapsed_ms": json.Number(strconv.FormatFloat(res.ElapsedMS, 'f', -1, 64)),
	}, nil
}

// doServiceCall is the direct Engine-B host seam for the same curated route
// contract Engine A executes in exec_service_call.go. The route is compiler-
// baked under __route; authored values can only fill escaped template params,
// and the ServiceCallClient remains fail-closed when no scoped token exists.
func doServiceCall(ctx context.Context, client *effects.ServiceCallClient, resolveRoute ServiceRouteResolver, budget *effects.StreamEgressLimiter, budgetKey string, config, inputs map[string]any) (map[string]any, error) {
	route, err := serviceCallRouteOf(config, resolveRoute)
	if err != nil {
		return nil, err
	}
	if budgetKey == "" {
		budgetKey = "bluehost"
	}
	if budget != nil && !budget.Allow(budgetKey) {
		return nil, fmt.Errorf("EGRESS_BUDGET_EXCEEDED")
	}
	path, err := effects.BuildPath(route.PathTemplate, route.Params, serviceParamsOf(inputs["params"]))
	if err != nil {
		return nil, fmt.Errorf("EGRESS_PARAM_MISSING: %w", err)
	}
	if client == nil {
		return nil, fmt.Errorf("SERVICE_CALL_UNCONFIGURED")
	}
	payload := bodyBytes(inputs["payload"])
	callCtx, cancel := context.WithTimeout(ctx, timeoutFrom(inputs["timeout_ms"]))
	defer cancel()
	result, err := client.Call(callCtx, route.Method, path, route.TokenPaths, payload)
	if err != nil {
		return nil, fmt.Errorf("SERVICE_CALL_FAILED: %w", err)
	}
	var body any
	if len(result.Body) > 0 {
		if err := json.Unmarshal(result.Body, &body); err != nil {
			body = string(result.Body)
		}
	}
	return map[string]any{
		"status": json.Number(strconv.Itoa(result.Status)),
		"body":   body,
		"ok":     result.Status >= 200 && result.Status <= 299,
	}, nil
}

func serviceCallRouteOf(config map[string]any, resolveRoute ServiceRouteResolver) (ServiceCallRoute, error) {
	raw, ok := config["__route"]
	if !ok {
		return ServiceCallRoute{}, fmt.Errorf("EGRESS_ROUTE_NOT_BAKED")
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return ServiceCallRoute{}, fmt.Errorf("EGRESS_ROUTE_INVALID")
	}
	// Blue's production ABI admits exactly this two-field reference. Full
	// method/path/token metadata is not a supported fallback: it is rejected
	// here as authored transport data and can never reach ServiceCallClient.
	if len(object) != 2 {
		return ServiceCallRoute{}, fmt.Errorf("EGRESS_ROUTE_INVALID")
	}
	service, serviceOK := object["service"].(string)
	routeID, routeOK := object["route_id"].(string)
	if !serviceOK || service == "" || !routeOK || routeID == "" {
		return ServiceCallRoute{}, fmt.Errorf("EGRESS_ROUTE_INVALID")
	}
	if resolveRoute == nil {
		return ServiceCallRoute{}, fmt.Errorf("EGRESS_ROUTE_UNRESOLVED: %s/%s", service, routeID)
	}
	route, found := resolveRoute(service, routeID)
	if !found {
		return ServiceCallRoute{}, fmt.Errorf("EGRESS_ROUTE_NOT_DECLARED: %s/%s", service, routeID)
	}
	if route.Service != service || route.RouteID != routeID || route.Method == "" || route.PathTemplate == "" {
		return ServiceCallRoute{}, fmt.Errorf("EGRESS_ROUTE_INVALID")
	}
	return route, nil
}

func serviceParamsOf(value any) map[string]string {
	params := map[string]string{}
	values, ok := value.(map[string]any)
	if !ok {
		return params
	}
	for name, value := range values {
		if text, ok := value.(string); ok {
			params[name] = text
			continue
		}
		encoded, err := json.Marshal(value)
		if err == nil {
			params[name] = string(encoded)
		}
	}
	return params
}

// strOf reads a string field from a decoded JSON `any` value — "" for
// anything absent or non-string, never a panic.
func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// bodyBytes marshals an authored `body` data input back into wire bytes —
// inputs arrive as decoded Go values (map[string]any/[]any/string/…), not
// raw JSON, so the outbound body is re-encoded rather than pulled verbatim.
func bodyBytes(v any) []byte {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return []byte(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// timeoutFrom reads `timeout_ms` through the shared transport bound; absent,
// zero, or negative values use the same default for both host adapters.
func timeoutFrom(v any) time.Duration {
	return httpTransportTimeout(v)
}
