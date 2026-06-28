package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZabLaboratory/Orion/internal/effects"
)

// The phase-3 async-effect ops (ADR 003 §3.1.3, issue #85):
// `http.request`, `db.query`, `source.read`. Each op runs its data
// pulls ON the scene goroutine, parks the chain's continuation under a
// version+epoch-stamped wake key (the #82/#83 mechanism), and submits
// the I/O to the bounded worker pool. On completion the worker calls
// scene.Input(InputMsg{ResumeExec, ResumeEnv}) — INTRA-PROCESS, never
// a `__system.*` write — and the continuation re-enters the effect
// node through the internal completion pin, binds its output pins into
// the task env, and continues down `then` or `error`.
//
// Doctrine §1.1: every failure mode (egress denial, timeout, undeclared
// source, full pool) is effect semantics on the `error` port — no task
// is ever killed; workers never touch scene state (single-writer holds).
//
// R9 dormancy: these ops are registered ONLY by Scene.SetEffects, which
// no production path calls — exec stays dormant on air until the
// phase-4 gate (#87).

// Exec op names for the async effects (runtime-canonical, like exec.go).
//
// source.read is NOT here: ADR 012 (Option B) reclassified
// core.source.read@1 from a world-touching exec effect to a pure compute
// (introspection of a compile-resolved descriptor — internal/runtime/
// compute_source.go). It carries no exec pins in the seed, so it was never
// reachable as an effect; the resolution now happens at compile.
const (
	OpHTTPRequest = "http.request"
	OpDBQuery     = "db.query"
)

// effectCompletePort is the internal exec in-pin a parked effect
// continuation re-enters its node through. Never wired by authored
// graphs (Blue port names don't use the `__` prefix).
const effectCompletePort = "__effect_complete"

// defaultEffectTimeout bounds an effect when the authored
// `timeout_seconds` is absent or non-positive.
const defaultEffectTimeout = 10 * time.Second

// maxEffectResponse bounds an http.request body read (same 1 MiB bound
// as the poller).
const maxEffectResponse = 1 << 20

// EffectMetrics is the phase-3 observability seam. *obs.Metrics
// implements it; nil disables it.
type EffectMetrics interface {
	// HTTPEgressBlocked counts an `http.request` egress-policy denial
	// (`orion_http_egress_blocked_total`).
	HTTPEgressBlocked(sceneID string)
	// EffectCompletionDropped counts a completion that could not be
	// delivered to the scene inbox (`orion_effect_completion_dropped_total`).
	EffectCompletionDropped(sceneID string)
	// EgressBudgetExceeded counts a `service.call` denied by the per-stream
	// egress budget (`orion_egress_budget_exceeded_total`) — the node fails
	// closed to its `error` port, never a crash (ADR Blue 009 §B / R3).
	EgressBudgetExceeded(sceneID string)
}

// SceneEffects bundles the executor dependencies of the async-effect
// ops. Built once at wiring time (phase 4 in prod; directly in tests)
// and shared across scenes — all fields are concurrency-safe.
type SceneEffects struct {
	// Runner is the bounded worker pool (required).
	Runner *effects.Runner
	// Egress is the `http.request` policy (required for http.request;
	// nil = deny-all, fail-closed).
	Egress *effects.EgressPolicy
	// DB is the topology-A `_query` client (required for db.query).
	DB *effects.DBQueryClient
	// ServiceCall is the curated service-egress client (ADR Blue 002;
	// required for service.call — nil = SERVICE_CALL_UNCONFIGURED on the
	// node's error port, never an anonymous call).
	ServiceCall *effects.ServiceCallClient
	// EgressBudget is the per-stream service.call rate-limit (ADR Blue 009
	// Amendment 2 §B / R3 — the bound the G0 clearance requires before a
	// WRITE route opens on the antenna path). nil = unbounded (dev /
	// unconfigured); production wires a positive default.
	EgressBudget *effects.StreamEgressLimiter
	// DataSources is the ORION_DATASOURCES allowlist.
	DataSources map[string]effects.DataSource
	// Metrics is the phase-3 metrics sink (nil = disabled).
	Metrics EffectMetrics
}

// worldEffectRegistrations is the SINGLE source of truth for the
// world-touching async-effect ops: the ops that open a socket, a DB
// connection, or read a live source. SetEffects installs EXACTLY these,
// and the B10 guard (worldEffectOps / ValidateValidationModeCoverage,
// exec_validation.go) derives from and reflects this same table. Adding a
// world-touching op means adding ONE entry here — which automatically
// lands it in both the validation-mode routing set AND the guard's
// coverage check, so it cannot be registered without a declared
// validation-mode synthetic response (the guard fails otherwise).
var worldEffectRegistrations = []struct {
	op string
	fn execOpFn
}{
	{OpHTTPRequest, execHTTPRequest},
	{OpDBQuery, execDBQuery},
	{OpServiceCall, execServiceCall},
}

// SetEffects installs the async-effect ops on this scene. Pre-Run only
// (like InstallExec / SetEffector). No production path calls this until
// the phase-4 gate (R9). It registers EXACTLY worldEffectRegistrations —
// the single table the B10 guard reflects against.
func (s *Scene) SetEffects(e *SceneEffects) {
	s.effects = e
	for _, r := range worldEffectRegistrations {
		s.registerExecOp(r.op, r.fn)
	}
}

// registeredWorldEffectOps installs SetEffects on a throwaway scene and
// reflects the world-touching ops it ACTUALLY registered, in sorted
// order. This is the introspective backbone of the B10 guard: it proves
// the guard's coverage check against the live registry SetEffects builds,
// not against a hand-maintained constant. A new world op reachable only
// through SetEffects therefore appears here automatically.
func registeredWorldEffectOps() []string {
	probe := &Scene{}
	probe.SetEffects(&SceneEffects{})
	out := make([]string, 0, len(probe.execOps))
	for op := range probe.execOps {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// ExecValidationError is a structural compile/install-time rejection
// of an exec program against deployment declarations.
type ExecValidationError struct {
	Code string // e.g. "DATASOURCE_NOT_DECLARED"
	Node string
	Name string
}

func (e *ExecValidationError) Error() string {
	return fmt.Sprintf("%s: node %q references %q", e.Code, e.Node, e.Name)
}

// ValidateExecDataSources checks every `db.query` node of a program
// against the declared DataSource allowlist (ADR 003 Amendment 1):
// an undeclared name fails with DATASOURCE_NOT_DECLARED. This is
// STRUCTURAL validation of authored config — `db.query` itself is
// always served (doctrine §1.1). The future compiler partition calls
// this at compile time; SetEffects' runtime guard backstops it.
func ValidateExecDataSources(p *ExecProgram, declared map[string]effects.DataSource) error {
	if p == nil {
		return nil
	}
	for _, n := range p.Nodes {
		if n.Op != OpDBQuery {
			continue
		}
		name := configString(n.Config, "datasource")
		if _, ok := declared[name]; !ok {
			return &ExecValidationError{Code: "DATASOURCE_NOT_DECLARED", Node: n.ID, Name: name}
		}
	}
	return nil
}

// --- shared park/submit/complete machinery ---------------------------

// effectEnvKey is the task-env slot the completion envelope lands in
// for a given effect node.
func effectEnvKey(nodeID string) string { return nodeID + ".__effect" }

// effectEnvelope is what travels from the worker to the resumed
// continuation through InputMsg.ResumeEnv.
type effectEnvelope struct {
	Value json.RawMessage `json:"value,omitempty"`
	Err   string          `json:"error,omitempty"`
}

// effectOutcome builds the park outcome for an effect node: continuation
// re-enters the node through effectCompletePort; start submits the job
// only once the park is accepted.
func (s *Scene) effectOutcome(node *ExecNode, key string, run func(ctx context.Context) effects.Result, timeout time.Duration) execOpOutcome {
	e := s.effects
	job := effects.Job{
		SceneID: s.id,
		Timeout: timeout,
		Run:     run,
		Deliver: func(res effects.Result) { s.deliverEffect(key, node.ID, res) },
	}
	return execOpOutcome{
		park:    true,
		parkKey: key,
		resume:  ExecTarget{Node: node.ID, Port: effectCompletePort},
		start: func() {
			if e == nil || e.Runner == nil || !e.Runner.Submit(job) {
				// Pool refused (full / stopped): fail the effect to its
				// error port synchronously — back-pressure, never a kill.
				s.logger.Warn("effect shed: worker pool refused job", "node", node.ID, "op", node.Op)
				s.resumeParkedWith(key, effectEnv(node.ID, effects.Result{Err: "EFFECT_QUEUE_FULL"}))
			}
		},
	}
}

// effectEnv marshals the completion envelope under the node's env slot.
func effectEnv(nodeID string, res effects.Result) map[string]json.RawMessage {
	raw, err := json.Marshal(effectEnvelope{Value: res.Value, Err: res.Err})
	if err != nil {
		raw = json.RawMessage(`{"error":"EFFECT_ENVELOPE_MARSHAL"}`)
	}
	return map[string]json.RawMessage{effectEnvKey(nodeID): raw}
}

// deliverEffect hands a completion to the scene loop — intra-process,
// through the same inbox as every input (FIFO, single-writer). Runs on
// a worker goroutine; touches nothing but the channel. A full inbox is
// retried briefly (the loop drains continuously); a persistent refusal
// is dropped LOUDLY (logged + counted) — the stamped wake key then
// stays parked until cancellation clears it.
func (s *Scene) deliverEffect(key, nodeID string, res effects.Result) {
	msg := InputMsg{
		ResumeExec: key,
		ResumeEnv:  effectEnv(nodeID, res),
		Source:     "system:effect/" + nodeID,
	}
	backoff := 5 * time.Millisecond
	for attempt := 0; attempt < 8; attempt++ {
		if s.Input(msg) {
			return
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	if s.effects != nil && s.effects.Metrics != nil {
		s.effects.Metrics.EffectCompletionDropped(s.id)
	}
	s.logger.Error("effect completion dropped: scene inbox full", "node", nodeID, "key", key)
}

// finishEffect runs when the continuation re-enters the effect node
// through effectCompletePort: bind output pins, route `then` vs `error`.
// bind maps the success Value onto the node's data-out pins.
func finishEffect(s *Scene, t *execTask, node *ExecNode, bind func(env map[string]json.RawMessage, value json.RawMessage)) execOpOutcome {
	var env effectEnvelope
	if raw, ok := t.env[effectEnvKey(node.ID)]; ok {
		_ = json.Unmarshal(raw, &env)
	} else {
		env.Err = "EFFECT_COMPLETION_MISSING"
	}
	delete(t.env, effectEnvKey(node.ID))
	if env.Err != "" {
		t.env[node.ID+".error"] = mustJSONString(env.Err)
		s.logger.Warn("effect failed", "node", node.ID, "op", node.Op, "err", env.Err)
		if tgt, ok := node.next("error"); ok {
			return execOpOutcome{next: &tgt}
		}
		// No error port wired: the chain ends here, loudly logged.
		return execOpOutcome{halt: true}
	}
	bind(t.env, env.Value)
	if tgt, ok := node.next("then", "completed"); ok {
		return execOpOutcome{next: &tgt}
	}
	return execOpOutcome{halt: true}
}

// mustJSONString marshals a string as a JSON value.
func mustJSONString(v string) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`"EFFECT_ERROR"`)
	}
	return raw
}

// effectTimeout reads the authored `timeout_seconds`. Non-positive —
// including negative values and IEEE-754 `-0`, which is == 0 and fails
// `> 0` — and NaN fall back to the default: a `-0` bound can never
// produce an instant or infinite timeout.
func (s *Scene) effectTimeout(t *execTask, node *ExecNode) time.Duration {
	secs := s.pullFloat(t, node, "timeout_seconds", 0)
	if !(secs > 0) { // negative, -0, 0, NaN
		return defaultEffectTimeout
	}
	d := durationFromSeconds(secs)
	if d <= 0 {
		return defaultEffectTimeout
	}
	return d
}

// effectTimeoutMillis reads the seed `timeout_ms` integer DATA input
// (ADR 010 §3.2). Non-positive — including IEEE-754 `-0`, negative, and
// NaN, all of which fail `> 0` — or an absent pin falls back to the
// effect default; the value is clamped to the hard ceiling maxEffectTimeout
// (Bastion §3.7 hardening (b)), so an authored timeout can never escape
// the worker-pool bound.
func (s *Scene) effectTimeoutMillis(t *execTask, node *ExecNode) time.Duration {
	ms := s.pullFloat(t, node, "timeout_ms", 0)
	if !(ms > 0) { // negative, -0, 0, NaN
		return defaultEffectTimeout
	}
	d := time.Duration(ms) * time.Millisecond
	if d <= 0 {
		return defaultEffectTimeout
	}
	if d > maxEffectTimeout {
		return maxEffectTimeout
	}
	return d
}

// --- http.request -----------------------------------------------------

// execHTTPRequest is the `http.request` op, the executor of the canonical
// seed node `core.http.request@1` (stdlib_seeder.py + Blue's preview
// executor `_http_request`). ADR 010 §3.2: the node is an exec effect with
// continuation — the seed declares exec pins `in`/`then`/`error`, so
// `isExecNode()` is true and the op is reachable from an authored graph
// (the pre-ADR-010 bug: no exec pins ⇒ data partition ⇒ unreachable).
//
// DATA inputs (all seed pins, method defaults GET): `url` / `method` /
// `query` / `headers` / `body` / `timeout_ms`. Outputs bind the seed pins
// `status` / `ok` / `body` / `headers` on `then`, and `error` on `error`.
//
// The egress policy (effects.EgressPolicy) is enforced unchanged in the
// worker: CheckURL (scheme + host allowlist, fail-closed) then post-DNS
// resolved-IP vetting at dial. This op adds CONTENT filtering on top of
// that transport policy — the three Bastion §3.7 hardenings:
//
//	(a) authored headers are filtered through dropForwardHeader: every
//	    sensitive credential header (Authorization, Cookie,
//	    Proxy-Authorization, any Proxy-*) and every hop-by-hop header
//	    (Host, Content-Length, Connection, Transfer-Encoding, Upgrade,
//	    TE, Trailer) is DROPPED, case-insensitively. Orion never forwards
//	    Authorization — a scene has no caller, and an authored
//	    `headers.Authorization` must not become an exfiltration channel for
//	    a secret read in-graph (asymmetry with Blue approved by Bastion,
//	    ADR 010 §3.7 / §5 D);
//	(b) cumulative outbound size is capped: query (maxOutboundQuery),
//	    headers (maxOutboundHeaders), body (maxOutboundBody); response read
//	    stays capped at maxEffectResponse; timeout_ms is clamped to
//	    maxEffectTimeout;
//	(c) logging is host-only — no header value, query value, or full URL
//	    ever reaches a log or metric (egressDenied already logs host-only
//	    via the wrapped error; this op adds no value-bearing log).
//
// Every failure mode (egress denial, cap exceeded, network, encode) binds
// `error` and fires the exec `error` pin — never a crash (ADR 003 §1.1).
func execHTTPRequest(s *Scene, t *execTask, node *ExecNode, inPort string) execOpOutcome {
	if inPort == effectCompletePort {
		return finishEffect(s, t, node, func(env map[string]json.RawMessage, value json.RawMessage) {
			var out struct {
				Status  int             `json:"status"`
				Body    json.RawMessage `json:"body"`
				Headers json.RawMessage `json:"headers"`
			}
			if err := json.Unmarshal(value, &out); err == nil {
				env[node.ID+".status"] = json.RawMessage(strconv.Itoa(out.Status))
				// Seed output pins: `body` (the response), `ok` (200..299),
				// and `headers` (flat record of the response headers).
				// `response` was the legacy `core.http-request@1` pin — not
				// in the canonical node.
				env[node.ID+".body"] = out.Body
				if len(out.Headers) > 0 {
					env[node.ID+".headers"] = out.Headers
				} else {
					env[node.ID+".headers"] = json.RawMessage(`{}`)
				}
				if out.Status >= 200 && out.Status <= 299 {
					env[node.ID+".ok"] = json.RawMessage(`true`)
				} else {
					env[node.ID+".ok"] = json.RawMessage(`false`)
				}
			}
		})
	}

	rawURL := pullString(s, t, node, "url")
	// Seed declares `method` as a DATA input (default GET), not config —
	// read it on demand like `url`.
	method := strings.ToUpper(pullString(s, t, node, "method"))
	if method == "" {
		method = http.MethodGet
	}
	// `query` and `headers` are JSON-object DATA inputs (string→scalar /
	// string→string). Absent pins read as nil and forward nothing.
	queryRaw, _ := s.pullData(t, node, "query")
	headersRaw, _ := s.pullData(t, node, "headers")
	body, _ := s.pullData(t, node, "body")
	timeout := s.effectTimeoutMillis(t, node)
	e := s.effects
	key := s.nextWakeKey()

	run := func(ctx context.Context) effects.Result {
		if e == nil || e.Egress == nil {
			return egressDenied(e, s.id, "", fmt.Errorf("%w: no egress policy configured (deny-all)", effects.ErrEgressBlocked))
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			// (c) host-only: url.Parse returns a *url.Error whose .Error()
			// re-echoes the raw URL (and thus any authored query secret). We
			// cannot trust a host from a string that failed to parse, so emit
			// a generic class with no raw value.
			return effects.Result{Err: "HTTP_REQUEST_INVALID_URL: malformed url"}
		}
		// (b) cap the outbound body before anything else.
		if len(body) > maxOutboundBody {
			return effects.Result{Err: "HTTP_REQUEST_BODY_TOO_LARGE"}
		}
		// Merge authored `query` into the URL query-string (scalar values
		// stringified); pre-existing params are preserved. (b) bounded.
		if err := mergeQuery(u, queryRaw); err != nil {
			return effects.Result{Err: err.Error()}
		}
		if err := e.Egress.CheckURL(u); err != nil {
			return egressDenied(e, s.id, u.Hostname(), err)
		}
		var reader io.Reader
		if len(body) > 0 {
			reader = strings.NewReader(string(body))
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
		if err != nil {
			// (c) host-only: http.NewRequest re-parses u.String() and its
			// error can re-echo the full URL (incl. authored query). Never
			// interpolate err.Error() raw.
			return effects.Result{Err: "HTTP_REQUEST_INVALID: " + httpFailureReason(u.Hostname(), err)}
		}
		if reader != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		// (a) forward authored headers through the sensitive/hop-by-hop
		// drop filter; (b) cap cumulative header size. Authored
		// Content-Type/Accept override the defaults set above.
		if err := applyAuthoredHeaders(req, headersRaw); err != nil {
			return effects.Result{Err: err.Error()}
		}
		resp, err := e.Egress.Client().Do(req)
		if err != nil {
			if errors.Is(err, effects.ErrEgressBlocked) {
				return egressDenied(e, s.id, u.Hostname(), err)
			}
			// (c) host-only: a *url.Error here carries the full request URL
			// (incl. authored query-string, which may hold an API key). Never
			// interpolate err.Error() raw — it would leak to the log and the
			// bound `error` pin. u.Hostname() is allowlisted/public.
			return effects.Result{Err: "HTTP_REQUEST_FAILED: " + httpFailureReason(u.Hostname(), err)}
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxEffectResponse))
		if err != nil {
			return effects.Result{Err: "HTTP_REQUEST_READ: " + err.Error()}
		}
		out, err := json.Marshal(struct {
			Status  int               `json:"status"`
			Body    json.RawMessage   `json:"body"`
			Headers map[string]string `json:"headers"`
		}{Status: resp.StatusCode, Body: asJSON(respBody), Headers: flattenHeaders(resp.Header)})
		if err != nil {
			return effects.Result{Err: "HTTP_REQUEST_ENCODE: " + err.Error()}
		}
		return effects.Result{Value: out}
	}
	return s.effectOutcome(node, key, run, timeout)
}

// outbound content caps (Bastion §3.7 hardening (b)). Sizes are bytes.
const (
	// maxOutboundHeaders bounds the cumulative authored header bytes
	// (name+value across all forwarded headers).
	maxOutboundHeaders = 16 << 10 // 16 KiB
	// maxOutboundQuery bounds the cumulative authored query-string bytes.
	maxOutboundQuery = 8 << 10 // 8 KiB
	// maxOutboundBody bounds the authored request body bytes.
	maxOutboundBody = 1 << 20 // 1 MiB
	// maxEffectTimeout is the hard ceiling on an authored timeout_ms.
	maxEffectTimeout = 60 * time.Second
)

// forbiddenForwardHeaders is the lower-cased set of headers an authored
// `headers` input may NEVER forward: sensitive credential headers and
// hop-by-hop headers. Proxy-* is matched by prefix (dropForwardHeader).
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

// dropForwardHeader reports whether an authored header name must be
// dropped before forwarding: case-insensitive membership in
// forbiddenForwardHeaders, or any `Proxy-*` header. Orion NEVER forwards
// Authorization (no caller in a scene — ADR 010 §3.2 / §3.7).
func dropForwardHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if _, ok := forbiddenForwardHeaders[lower]; ok {
		return true
	}
	return strings.HasPrefix(lower, "proxy-")
}

// applyAuthoredHeaders sets the authored `headers` object on the request,
// dropping sensitive/hop-by-hop names (a) and enforcing the cumulative
// header cap (b). A non-object `headers` input is ignored (nothing
// forwarded), not an error — an unwired/odd data pin must not fail egress.
func applyAuthoredHeaders(req *http.Request, headersRaw json.RawMessage) error {
	if len(headersRaw) == 0 {
		return nil
	}
	var authored map[string]json.RawMessage
	if err := json.Unmarshal(headersRaw, &authored); err != nil {
		return nil
	}
	total := 0
	for name, valRaw := range authored {
		if dropForwardHeader(name) {
			continue
		}
		val := scalarToString(valRaw)
		total += len(name) + len(val)
		if total > maxOutboundHeaders {
			return errors.New("HTTP_REQUEST_HEADERS_TOO_LARGE")
		}
		req.Header.Set(name, val)
	}
	return nil
}

// mergeQuery folds an authored `query` object into the URL query-string,
// stringifying scalar values and preserving any params already on the URL.
// Enforces the query cap (b). A non-object input is ignored.
func mergeQuery(u *url.URL, queryRaw json.RawMessage) error {
	if len(queryRaw) == 0 {
		return nil
	}
	var authored map[string]json.RawMessage
	if err := json.Unmarshal(queryRaw, &authored); err != nil {
		return nil
	}
	q := u.Query()
	for k, vRaw := range authored {
		q.Set(k, scalarToString(vRaw))
	}
	encoded := q.Encode()
	if len(encoded) > maxOutboundQuery {
		return errors.New("HTTP_REQUEST_QUERY_TOO_LARGE")
	}
	u.RawQuery = encoded
	return nil
}

// scalarToString renders a JSON scalar as its string form for a header or
// query value: a JSON string yields its contents; any other scalar yields
// its compact JSON text. Keeps `42` → "42", `"x"` → "x".
func scalarToString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// flattenHeaders renders response headers as a flat string→string record
// (last value wins on repeats), matching Blue's response `headers` shape.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			out[k] = vs[len(vs)-1]
		}
	}
	return out
}

// httpFailureReason renders a transport failure as a host-only string,
// never the full request URL. A *url.Error from net/http carries the
// complete URL (`Get "https://host/path?api_key=SECRET": <cause>`); its
// .Error() must NEVER reach a log or a bound `error` pin. We bind only the
// authorized host (already public, allowlisted) plus an unwrapped cause that
// no longer holds the URL. (c) logging host-only.
func httpFailureReason(host string, err error) string {
	cause := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// urlErr.Err is the underlying transport error (DNS, connrefused,
		// TLS, timeout) — it does not carry the URL/query that urlErr.Error()
		// prepends. Fall back to a generic class only if it somehow does.
		if urlErr.Err != nil {
			cause = urlErr.Err.Error()
		} else {
			cause = "request failed"
		}
	}
	if host == "" {
		return cause
	}
	return host + ": " + cause
}

// egressDenied counts the policy denial and shapes the error result.
//
// (c) host-only: a denial raised in the transport (the dial-time IP guard
// closing SSRF / DNS-rebinding, or a redirect re-check) is wrapped by
// net/http into a *url.Error whose .Error() re-echoes the full request URL —
// `Get "https://host/path?api_key=SECRET": <cause>` — so it carries any
// authored query secret. errors.Is(err, ErrEgressBlocked) survives that wrap,
// so we reach here with a URL-bearing error. Route it through the same
// host-only sanitisation as the other failure sites (httpFailureReason):
// `host` is the allowlisted/public hostname, never the path or query. Pre-flight
// denials (CheckURL: no policy, host not allowlisted) carry no URL and pass an
// empty host — still sanitised for uniformity.
func egressDenied(e *SceneEffects, sceneID, host string, err error) effects.Result {
	if e != nil && e.Metrics != nil {
		e.Metrics.HTTPEgressBlocked(sceneID)
	}
	return effects.Result{Err: "EGRESS_BLOCKED: " + httpFailureReason(host, err)}
}

// asJSON passes valid JSON through verbatim and wraps anything else as
// a JSON string, so a non-JSON body still binds to the response pin.
func asJSON(b []byte) json.RawMessage {
	if json.Valid(b) && len(strings.TrimSpace(string(b))) > 0 {
		return json.RawMessage(b)
	}
	raw, err := json.Marshal(string(b))
	if err != nil {
		return json.RawMessage(`null`)
	}
	return raw
}

// --- db.query ----------------------------------------------------------

// execDBQuery is the `db.query` op (topology A). Inputs: `datasource`
// (config), `descriptor` (data/config — the QueryMe QueryDescriptor),
// `timeout_seconds`. Outputs: `<node>.rows`, `<node>.count`,
// `<node>.elapsed_ms` on `then`; `<node>.error` on `error`.
func execDBQuery(s *Scene, t *execTask, node *ExecNode, inPort string) execOpOutcome {
	if inPort == effectCompletePort {
		return finishEffect(s, t, node, func(env map[string]json.RawMessage, value json.RawMessage) {
			var out effects.QueryResult
			if err := json.Unmarshal(value, &out); err == nil {
				env[node.ID+".rows"] = out.Rows
				env[node.ID+".count"] = json.RawMessage(strconv.Itoa(out.Count))
				env[node.ID+".elapsed_ms"] = json.RawMessage(strconv.FormatFloat(out.ElapsedMS, 'f', -1, 64))
			}
		})
	}

	name := configString(node.Config, "datasource")
	descriptor, ok := s.pullData(t, node, "descriptor")
	if !ok {
		descriptor = json.RawMessage(`{}`)
	}
	timeout := s.effectTimeout(t, node)
	e := s.effects
	key := s.nextWakeKey()

	run := func(ctx context.Context) effects.Result {
		if e == nil || e.DB == nil {
			return effects.Result{Err: "DB_QUERY_UNCONFIGURED: no _query client"}
		}
		ds, declared := e.DataSources[name]
		if !declared {
			// Backstop of the compile-time ValidateExecDataSources gate
			// (structural — db.query stays served, the DECLARATION is
			// what is missing).
			return effects.Result{Err: "DATASOURCE_NOT_DECLARED: " + name}
		}
		res, err := e.DB.Query(ctx, ds, descriptor)
		if err != nil {
			return effects.Result{Err: "DB_QUERY_FAILED: " + err.Error()}
		}
		out, err := json.Marshal(res)
		if err != nil {
			return effects.Result{Err: "DB_QUERY_ENCODE: " + err.Error()}
		}
		return effects.Result{Value: out}
	}
	return s.effectOutcome(node, key, run, timeout)
}

// pullString reads a string data input (url). Unwired/invalid → "".
func pullString(s *Scene, t *execTask, node *ExecNode, port string) string {
	raw, ok := s.pullData(t, node, port)
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		s.logger.Warn("exec: data input not a string", "node", node.ID, "port", port, "value", string(raw))
		return ""
	}
	return v
}
