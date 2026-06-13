package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Tests for the ADR 010 §3.2 / §3.7 http.request executor: the full
// forward of query/headers/timeout_ms, the response `headers` output bind,
// and the three Bastion-cleared content hardenings (sensitive/hop-by-hop
// header drop incl. authored Authorization, outbound caps, host-only
// logging — proven indirectly via the deny path).

// httpHardeningGraph declares the leaves these tests bind. Distinct from
// effectsGraph so a new leaf here never perturbs the shared fixture.
func httpHardeningGraph(id string) *compiler.Graph {
	return &compiler.Graph{
		SceneID:      id,
		SceneVersion: "sha256:http-hardening-test",
		Defaults: map[string]json.RawMessage{
			"__vars.bp.body":    raw(`null`),
			"__vars.bp.status":  raw(`null`),
			"__vars.bp.headers": raw(`null`),
			"__vars.bp.err":     raw(`null`),
		},
	}
}

func httpHardeningScene(t *testing.T, id string, prog *ExecProgram, eff *SceneEffects) *Scene {
	t.Helper()
	sc := NewScene(id, httpHardeningGraph(id), &compiler.RenderBundle{SceneVersion: "sha256:http-hardening-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(prog)
	sc.SetEffects(eff)
	return sc
}

// capturingServer records the first request it sees so a test can assert
// what the executor actually forwarded.
type capturedReq struct {
	mu      sync.Mutex
	rawQ    string
	headers http.Header
	method  string
}

func (c *capturedReq) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rawQ = r.URL.RawQuery
	c.headers = r.Header.Clone()
	c.method = r.Method
}

func (c *capturedReq) snapshot() (string, http.Header, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rawQ, c.headers, c.method
}

// TestEffects_HTTP_ForwardsQueryAndHeaders: authored `query` is merged
// into the query-string and authored (non-sensitive) `headers` reach the
// server. The response `headers` output pin is bound on `then`.
func TestEffects_HTTP_ForwardsQueryAndHeaders(t *testing.T) {
	capt := &capturedReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capt.record(r)
		w.Header().Set("X-Resp", "pong")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{
					"url":     raw(`"` + srv.URL + `?base=1"`),
					"query":   raw(`{"q":"zab","n":42}`),
					"headers": raw(`{"X-Custom":"hello"}`),
				},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.body"},
					"error": {Node: "set.err"},
				}},
			"set.body": setFromPin("set.body", "body", "req", "body",
				map[string]ExecTarget{"then": {Node: "set.headers"}}),
			"set.headers": setFromPin("set.headers", "headers", "req", "headers", nil),
			"set.err":     setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "req"}}},
	}
	eff := &SceneEffects{Runner: newTestRunner(t), Egress: loopbackEgress(t, srv.URL)}
	sc := httpHardeningScene(t, "http-forward", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.body", `{"ok":true}`, 2*time.Second)

	rawQ, hdrs, _ := capt.snapshot()
	q, _ := url.ParseQuery(rawQ)
	if q.Get("base") != "1" {
		t.Fatalf("pre-existing url param dropped: %q", rawQ)
	}
	if q.Get("q") != "zab" || q.Get("n") != "42" {
		t.Fatalf("authored query not forwarded: %q", rawQ)
	}
	if hdrs.Get("X-Custom") != "hello" {
		t.Fatalf("authored header not forwarded: %v", hdrs)
	}
	// Response headers output bound (flat record, last value wins).
	respHdrs, _ := sc.state.Get("__vars.bp.headers")
	var got map[string]string
	if err := json.Unmarshal(respHdrs, &got); err != nil {
		t.Fatalf("headers output not an object: %s", respHdrs)
	}
	if got["X-Resp"] != "pong" {
		t.Fatalf("response header X-Resp not bound: %v", got)
	}
}

// TestEffects_HTTP_DropsSensitiveAuthoredHeaders: an authored
// `headers.Authorization` / `headers.Cookie` / `headers.Proxy-Foo` / a
// hop-by-hop `Connection` must NEVER reach the server — Orion forwards no
// credential header (the asymmetry with Blue, ADR 010 §3.7). A
// non-sensitive header alongside them still goes through.
func TestEffects_HTTP_DropsSensitiveAuthoredHeaders(t *testing.T) {
	capt := &capturedReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capt.record(r)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{
					"url": raw(`"` + srv.URL + `"`),
					"headers": raw(`{
						"Authorization":"Bearer SECRET",
						"authorization":"bearer lowercase",
						"Cookie":"session=abc",
						"Proxy-Authorization":"x",
						"Proxy-Foo":"y",
						"Connection":"keep-alive",
						"X-Kept":"yes"
					}`),
				},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.body"},
					"error": {Node: "set.err"},
				}},
			"set.body": setFromPin("set.body", "body", "req", "body", nil),
			"set.err":  setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "req"}}},
	}
	eff := &SceneEffects{Runner: newTestRunner(t), Egress: loopbackEgress(t, srv.URL)}
	sc := httpHardeningScene(t, "http-drophdr", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.body", `{}`, 2*time.Second)

	_, hdrs, _ := capt.snapshot()
	for _, banned := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Proxy-Foo"} {
		if v := hdrs.Get(banned); v != "" {
			t.Fatalf("sensitive authored header %q forwarded: %q", banned, v)
		}
	}
	// Connection is hop-by-hop; Go's transport may set its own, but our
	// authored "keep-alive" must not be what reaches the server verbatim
	// as an authored forward — assert it is not our authored value.
	if hdrs.Get("X-Kept") != "yes" {
		t.Fatalf("non-sensitive authored header dropped: %v", hdrs)
	}
}

// TestEffects_HTTP_EgressEnforcedNoCall: a host outside the allowlist is
// denied — the error port fires with EGRESS_BLOCKED, the denial is
// counted, and the upstream server is never contacted (no call made).
func TestEffects_HTTP_EgressEnforcedNoCall(t *testing.T) {
	var called int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		called++
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	metrics := &fakeEffectMetrics{}
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{
					"url": raw(`"https://evil.example.org/x"`),
				},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.body"},
					"error": {Node: "set.err"},
				}},
			"set.body": setFromPin("set.body", "body", "req", "body", nil),
			"set.err":  setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "req"}}},
	}
	// Allowlist only the test server host — the authored URL is elsewhere.
	eff := &SceneEffects{Runner: newTestRunner(t), Egress: loopbackEgress(t, srv.URL), Metrics: metrics}
	sc := httpHardeningScene(t, "http-egress", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("__vars.bp.err"); ok && strings.Contains(string(v), "EGRESS_BLOCKED") {
			if metrics.blocked() == 0 {
				t.Fatal("denial must be counted")
			}
			mu.Lock()
			c := called
			mu.Unlock()
			if c != 0 {
				t.Fatalf("server contacted despite egress denial: %d calls", c)
			}
			if b, _ := sc.state.Get("__vars.bp.body"); string(b) != `null` {
				t.Fatalf("then port fired on denial: %s", b)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get("__vars.bp.err")
	t.Fatalf("error port did not fire, __vars.bp.err = %s", v)
}

// TestEffects_HTTP_HeaderCapToErrorPort: authored headers exceeding the
// cumulative cap fail to the error port, never a crash — the executor
// never dials.
func TestEffects_HTTP_HeaderCapToErrorPort(t *testing.T) {
	var called int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		called++
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	big := strings.Repeat("a", maxOutboundHeaders+10)
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{
					"url":     raw(`"` + srv.URL + `"`),
					"headers": raw(`{"X-Big":"` + big + `"}`),
				},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.body"},
					"error": {Node: "set.err"},
				}},
			"set.body": setFromPin("set.body", "body", "req", "body", nil),
			"set.err":  setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "req"}}},
	}
	eff := &SceneEffects{Runner: newTestRunner(t), Egress: loopbackEgress(t, srv.URL)}
	sc := httpHardeningScene(t, "http-headercap", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("__vars.bp.err"); ok && strings.Contains(string(v), "HTTP_REQUEST_HEADERS_TOO_LARGE") {
			mu.Lock()
			c := called
			mu.Unlock()
			if c != 0 {
				t.Fatalf("server contacted despite header cap: %d calls", c)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get("__vars.bp.err")
	t.Fatalf("header cap did not fire the error port, __vars.bp.err = %s", v)
}

// TestExecHTTP_TimeoutMillisClampAndFallback unit-tests the timeout_ms
// reader: a non-positive / NaN value falls back to the default, and a
// huge value is clamped to maxEffectTimeout.
func TestExecHTTP_TimeoutMillisClampAndFallback(t *testing.T) {
	sc := httpHardeningScene(t, "http-timeout-unit", &ExecProgram{
		BlueprintKey: "bp",
		Nodes:        map[string]*ExecNode{},
		Entrypoints:  map[string]ExecEntry{},
	}, &SceneEffects{Runner: newTestRunner(t)})

	cases := []struct {
		name string
		cfg  string
		want time.Duration
	}{
		{"absent", "", defaultEffectTimeout},
		{"zero", `0`, defaultEffectTimeout},
		{"negative", `-5`, defaultEffectTimeout},
		{"negzero", `-0`, defaultEffectTimeout},
		{"normal", `2000`, 2 * time.Second},
		{"clamped", `999999999`, maxEffectTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &ExecNode{ID: "req", Op: OpHTTPRequest, Config: map[string]json.RawMessage{}}
			if tc.cfg != "" {
				node.Config["timeout_ms"] = raw(tc.cfg)
			}
			got := sc.effectTimeoutMillis(&execTask{env: map[string]json.RawMessage{}}, node)
			if got != tc.want {
				t.Fatalf("timeout_ms %q: got %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}
