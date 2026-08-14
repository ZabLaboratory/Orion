package bluehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// EffectDeps bundles the host-side transport clients NewEffectHandlers wires
// into Engine B's 3 opcodes of full right (ENGINE-B-PARITY-BLUE:
// core.http.request@1, core.http-request@1, core.db.query@1 — the opcodes
// walker.go dispatches through StartOptions.EffectHandlers, NOT the
// core.effect.invoke@1 async admission protocol). deps is the SAME
// *effects.EgressPolicy / *effects.DBQueryClient instance cmd/orion/main.go
// wires into Engine A's SceneEffects, so both engines are bound by one
// allowlist and one service token — never a second independent transport
// stack.
type EffectDeps struct {
	Egress      *effects.EgressPolicy
	DataSources map[string]effects.DataSource
	DB          *effects.DBQueryClient
}

const (
	// maxHostEffectResponse bounds an http.request body read — same 1 MiB
	// bound as Engine A's maxEffectResponse (exec_effects.go).
	maxHostEffectResponse = 1 << 20
	// defaultHostEffectTimeout mirrors Engine A's defaultEffectTimeout.
	defaultHostEffectTimeout = 10 * time.Second
	// maxHostEffectTimeout mirrors Engine A's maxEffectTimeout ceiling.
	maxHostEffectTimeout = 60 * time.Second
)

// forbiddenForwardHeaders is Engine A's exact deny-list (exec_effects.go
// forbiddenForwardHeaders) — duplicated here because the source list is
// unexported from internal/runtime and internal/runtime must not import
// internal/bluehost (no reverse dependency onto the engine it parity-tests
// against). Flagged for Conduit/Bastion: a shared internal/effects home for
// this constant would remove the duplication; not done unilaterally here
// since it touches Engine A's file (exclusion: "Engine A reste intact").
var forbiddenForwardHeaders = map[string]struct{}{
	"authorization":       {},
	"cookie":              {},
	"proxy-authorization": {},
	"host":                {},
	"content-length":      {},
	"connection":          {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"te":                  {},
	"trailer":             {},
}

func dropForwardHeader(name string) bool {
	lower := strings.ToLower(name)
	if _, denied := forbiddenForwardHeaders[lower]; denied {
		return true
	}
	return strings.HasPrefix(lower, "proxy-")
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
	http := func(config, inputs map[string]any) (map[string]any, error) {
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
	if egress == nil {
		return nil, fmt.Errorf("EFFECT_PROVIDER_UNAVAILABLE: no egress policy configured (deny-all)")
	}
	rawURL, _ := inputs["url"].(string)
	method := strings.ToUpper(strOf(inputs["method"]))
	if method == "" {
		method = http.MethodGet
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("HTTP_REQUEST_INVALID_URL: malformed url")
	}
	if err := mergeQuery(u, inputs["query"]); err != nil {
		return nil, err
	}
	if err := egress.CheckURL(u); err != nil {
		return nil, fmt.Errorf("EGRESS_BLOCKED: %s: %w", u.Hostname(), err)
	}
	var reader io.Reader
	if body := bodyBytes(inputs["body"]); len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, timeoutFrom(inputs["timeout_ms"]))
	defer cancel()
	req, err := http.NewRequestWithContext(timeoutCtx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("HTTP_REQUEST_INVALID: %s", httpFailureReason(u.Hostname(), err))
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if err := applyAuthoredHeaders(req, inputs["headers"]); err != nil {
		return nil, err
	}
	resp, err := egress.Client().Do(req)
	if err != nil {
		if errors.Is(err, effects.ErrEgressBlocked) {
			return nil, fmt.Errorf("EGRESS_BLOCKED: %s: %w", u.Hostname(), err)
		}
		return nil, fmt.Errorf("HTTP_REQUEST_FAILED: %s", httpFailureReason(u.Hostname(), err))
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxHostEffectResponse))
	if err != nil {
		return nil, fmt.Errorf("HTTP_REQUEST_READ: %w", err)
	}
	var bodyValue any
	if len(respBody) > 0 {
		if jsonErr := json.Unmarshal(respBody, &bodyValue); jsonErr != nil {
			bodyValue = string(respBody)
		}
	}
	return map[string]any{
		"status":  json.Number(strconv.Itoa(resp.StatusCode)),
		"body":    bodyValue,
		"headers": flattenHeaders(resp.Header),
		"ok":      resp.StatusCode >= 200 && resp.StatusCode <= 299,
	}, nil
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

// timeoutFrom reads `timeout_ms`, clamped to (0, maxHostEffectTimeout] —
// absent/zero/negative falls back to defaultHostEffectTimeout, mirroring
// Engine A's effectTimeoutMillis fallback.
func timeoutFrom(v any) time.Duration {
	ms := numOf(v)
	if ms <= 0 {
		return defaultHostEffectTimeout
	}
	d := time.Duration(ms) * time.Millisecond
	if d > maxHostEffectTimeout {
		return maxHostEffectTimeout
	}
	return d
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

// mergeQuery merges an authored `query` object (string -> scalar) into
// u's query string — pre-existing params are preserved, mirroring Engine
// A's mergeQuery (exec_effects.go).
func mergeQuery(u *url.URL, raw any) error {
	obj, ok := raw.(map[string]any)
	if !ok || len(obj) == 0 {
		return nil
	}
	q := u.Query()
	for key, value := range obj {
		q.Set(key, scalarToString(value))
	}
	u.RawQuery = q.Encode()
	return nil
}

func scalarToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// applyAuthoredHeaders forwards an authored `headers` object (string ->
// scalar) onto req, dropping sensitive/hop-by-hop headers
// (dropForwardHeader) — Engine A's applyAuthoredHeaders (exec_effects.go)
// hardening (b)/(c), same deny-list.
func applyAuthoredHeaders(req *http.Request, raw any) error {
	obj, ok := raw.(map[string]any)
	if !ok || len(obj) == 0 {
		return nil
	}
	for name, value := range obj {
		if dropForwardHeader(name) {
			continue
		}
		req.Header.Set(name, scalarToString(value))
	}
	return nil
}

func flattenHeaders(h http.Header) map[string]any {
	out := make(map[string]any, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// httpFailureReason is host-only: never interpolates the raw error (which
// can re-echo the authored URL/query, a potential secret-exfiltration
// channel) — same discipline as Engine A's httpFailureReason.
func httpFailureReason(host string, err error) string {
	return fmt.Sprintf("request to %s failed: %s", host, classifyNetErr(err))
}

func classifyNetErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, effects.ErrEgressBlocked):
		return "egress blocked"
	default:
		return "transport error"
	}
}
