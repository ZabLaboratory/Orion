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
// into Engine B's 3 opcodes of full right (ENGINE-B-PARITY-BLUE:
// core.http.request@1, core.http-request@1, core.db.query@1 — the opcodes
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
}

// NewEffectHandlers builds the StartOptions.EffectHandlers table for one
// instance. mode gates the transport: blueruntime.Preview NEVER dials the
// network or the DB — CheckURL/DB.Query are not even reached, proven by
// construction, satisfying issue #358 §7's explicit preview criterion —
// while blueruntime.Execute dispatches for real through deps, the same
// policy (allowlist, token, response caps) Engine A's on-air scene uses.
//
// This is a DELIBERATE divergence from Engine A's OWN preview.go clone,
// which makes real bounded egress in preview (a per-stream budget, not a
// no-op — see preview.go's SetEffects doc comment). The issue's acceptance
// criteria are explicit and testable ("aucune requête... émise" in preview);
// Engine A's actual runtime behaviour contradicts them. This function
// implements the ISSUE's stated contract; the discrepancy itself is
// reported, not resolved unilaterally (an architecture call for
// Atlas/Conduit, not a Forge judgment call).
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
	return map[string]blueruntime.EffectFunc{
		"core.http.request@1": http,
		"core.http-request@1": http,
		"core.db.query@1":     db,
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

// numOf decodes a JSON number arriving as json.Number (the portable ABI's
// decode convention, program.go's jsonNumber) or float64 (a Go-literal
// input in tests) — 0 for anything else.
func numOf(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}
