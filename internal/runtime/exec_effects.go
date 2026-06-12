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
const (
	OpHTTPRequest = "http.request"
	OpDBQuery     = "db.query"
	OpSourceRead  = "source.read"
)

// effectCompletePort is the internal exec in-pin a parked effect
// continuation re-enters its node through. Never wired by authored
// graphs (Blue port names don't use the `__` prefix).
const effectCompletePort = "__effect_complete"

// defaultEffectTimeout bounds an effect when the authored
// `timeout_seconds` is absent or non-positive.
const defaultEffectTimeout = 10 * time.Second

// maxEffectResponse bounds an http.request / source.read body read
// (same 1 MiB bound as the poller).
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
	// DataSources is the ORION_DATASOURCES allowlist.
	DataSources map[string]effects.DataSource
	// SourceClient performs `source.read` fetches of DECLARED adapter
	// sources (operator-declared URLs — the same trust level as the
	// poller, which is why it is a plain client and not the egress
	// one). nil = a default client.
	SourceClient *http.Client
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
	{OpSourceRead, execSourceRead},
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

// --- http.request -----------------------------------------------------

// execHTTPRequest is the `http.request` op, aligned to the canonical
// seed node `core.http.request@1` (stdlib_seeder.py + Blue's preview
// executor `_http_request`): `url` / `method` / `body` are DATA inputs
// (method defaults GET); outputs bind the seed pins `status` / `ok` /
// `body` on `then` and `error` on `error`. The egress policy is enforced
// in the worker: URL check, then post-DNS resolved-IP vetting at dial.
//
// NOTE (handed to Eleven — out of this rename's scope): the seed also
// declares `query` / `headers` DATA inputs and a `timeout_ms` integer,
// none of which this op forwards yet (it reads the shared
// `timeout_seconds`). Wiring query/headers is new HTTP egress surface —
// a Bastion-cleared change, not a port rename — and is deliberately NOT
// done here. Until then a blueprint's query/headers pins are silently
// dropped; the parity gate flags only port-NAME drift, not this
// behavioural gap, which is documented as a known follow-up.
func execHTTPRequest(s *Scene, t *execTask, node *ExecNode, inPort string) execOpOutcome {
	if inPort == effectCompletePort {
		return finishEffect(s, t, node, func(env map[string]json.RawMessage, value json.RawMessage) {
			var out struct {
				Status int             `json:"status"`
				Body   json.RawMessage `json:"body"`
			}
			if err := json.Unmarshal(value, &out); err == nil {
				env[node.ID+".status"] = json.RawMessage(strconv.Itoa(out.Status))
				// Seed output pins: `body` (the response) and `ok`
				// (200..299). `response` was the legacy `core.http-request@1`
				// pin name — not in the canonical node.
				env[node.ID+".body"] = out.Body
				if out.Status >= 200 && out.Status <= 299 {
					env[node.ID+".ok"] = json.RawMessage(`true`)
				} else {
					env[node.ID+".ok"] = json.RawMessage(`false`)
				}
			}
		})
	}

	rawURL := pullString(s, t, node, "url")
	// Seed `core.http.request@1` declares `method` as a DATA input
	// (default GET), not config — read it on demand like `url`.
	method := strings.ToUpper(pullString(s, t, node, "method"))
	if method == "" {
		method = http.MethodGet
	}
	body, _ := s.pullData(t, node, "body")
	timeout := s.effectTimeout(t, node)
	e := s.effects
	key := s.nextWakeKey()

	run := func(ctx context.Context) effects.Result {
		if e == nil || e.Egress == nil {
			return egressDenied(e, s.id, fmt.Errorf("%w: no egress policy configured (deny-all)", effects.ErrEgressBlocked))
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return effects.Result{Err: "HTTP_REQUEST_INVALID_URL: " + err.Error()}
		}
		if err := e.Egress.CheckURL(u); err != nil {
			return egressDenied(e, s.id, err)
		}
		var reader io.Reader
		if len(body) > 0 {
			reader = strings.NewReader(string(body))
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
		if err != nil {
			return effects.Result{Err: "HTTP_REQUEST_INVALID: " + err.Error()}
		}
		if reader != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		resp, err := e.Egress.Client().Do(req)
		if err != nil {
			if errors.Is(err, effects.ErrEgressBlocked) {
				return egressDenied(e, s.id, err)
			}
			return effects.Result{Err: "HTTP_REQUEST_FAILED: " + err.Error()}
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxEffectResponse))
		if err != nil {
			return effects.Result{Err: "HTTP_REQUEST_READ: " + err.Error()}
		}
		out, err := json.Marshal(struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		}{Status: resp.StatusCode, Body: asJSON(respBody)})
		if err != nil {
			return effects.Result{Err: "HTTP_REQUEST_ENCODE: " + err.Error()}
		}
		return effects.Result{Value: out}
	}
	return s.effectOutcome(node, key, run, timeout)
}

// egressDenied counts the policy denial and shapes the error result.
func egressDenied(e *SceneEffects, sceneID string, err error) effects.Result {
	if e != nil && e.Metrics != nil {
		e.Metrics.HTTPEgressBlocked(sceneID)
	}
	return effects.Result{Err: "EGRESS_BLOCKED: " + err.Error()}
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

// --- source.read --------------------------------------------------------

// execSourceRead is the `source.read` op: an on-demand fetch of a
// DECLARED external source — the graph binding (`external_adapter`)
// whose `key` matches the authored `source_id` config. The URL is the
// operator/author-declared binding URL (the same trust level the
// poller already fetches on a cadence), NOT a blueprint-computed
// destination — hence the plain client. Outputs: `<node>.value` on
// `then`; `<node>.error` on `error`. An undeclared source fails to the
// error port (`SOURCE_NOT_DECLARED`) — structural, never a capability
// refusal.
func execSourceRead(s *Scene, t *execTask, node *ExecNode, inPort string) execOpOutcome {
	if inPort == effectCompletePort {
		return finishEffect(s, t, node, func(env map[string]json.RawMessage, value json.RawMessage) {
			env[node.ID+".value"] = value
		})
	}

	// Seed `core.source.read@1` declares its config key as `source_id`
	// (stdlib_seeder.py) — the UUID of the data_sources row to read.
	name := configString(node.Config, "source_id")
	var srcURL string
	for _, b := range s.graph.Bindings {
		if b.Key == name && b.URL != "" {
			srcURL = b.URL
			break
		}
	}
	timeout := s.effectTimeout(t, node)
	e := s.effects
	key := s.nextWakeKey()

	run := func(ctx context.Context) effects.Result {
		if srcURL == "" {
			return effects.Result{Err: "SOURCE_NOT_DECLARED: " + name}
		}
		client := http.DefaultClient
		if e != nil && e.SourceClient != nil {
			client = e.SourceClient
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
		if err != nil {
			return effects.Result{Err: "SOURCE_READ_INVALID: " + err.Error()}
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return effects.Result{Err: "SOURCE_READ_FAILED: " + err.Error()}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return effects.Result{Err: fmt.Sprintf("SOURCE_READ_STATUS: %d", resp.StatusCode)}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxEffectResponse))
		if err != nil {
			return effects.Result{Err: "SOURCE_READ_READ: " + err.Error()}
		}
		return effects.Result{Value: asJSON(body)}
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
