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
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// Tests for the phase-3 async-effect ops (ADR 003 §3.1.3, issue #85):
// off-goroutine execution on the bounded pool, intra-process completion
// via InputMsg{ResumeExec, ResumeEnv} (never a __system.* write),
// egress fail-closed, db.query topology A, source.read of declared
// bindings, and every failure mode on the `error` port — no kill.

type fakeEffectMetrics struct {
	mu             sync.Mutex
	egressBlocked  int
	complDropped   int
}

func (f *fakeEffectMetrics) HTTPEgressBlocked(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.egressBlocked++
}

func (f *fakeEffectMetrics) EffectCompletionDropped(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.complDropped++
}

func (f *fakeEffectMetrics) blocked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.egressBlocked
}

// effectsGraph declares the `__vars.bp.*` leaves the test chains write.
func effectsGraph(id string, bindings ...compiler.ExternalAdapter) *compiler.Graph {
	return &compiler.Graph{
		SceneID:      id,
		SceneVersion: "sha256:effects-test",
		Bindings:     bindings,
		Defaults: map[string]json.RawMessage{
			"__vars.bp.out":    raw(`null`),
			"__vars.bp.out2":   raw(`null`),
			"__vars.bp.status": raw(`null`),
			"__vars.bp.err":    raw(`null`),
			"__vars.bp.after":  raw(`null`),
		},
	}
}

func newTestRunner(t *testing.T) *effects.Runner {
	t.Helper()
	r := effects.NewRunner(4, 64, quietLogger())
	r.Start()
	t.Cleanup(r.Stop)
	return r
}

func effectsScene(t *testing.T, id string, prog *ExecProgram, eff *SceneEffects, bindings ...compiler.ExternalAdapter) *Scene {
	t.Helper()
	sc := NewScene(id, effectsGraph(id, bindings...), &compiler.RenderBundle{SceneVersion: "sha256:effects-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(prog)
	sc.SetEffects(eff)
	return sc
}

// loopbackEgress allowlists the httptest server's host. The
// InsecureAllowPrivateForTest hook exists ONLY because httptest binds
// loopback — the policy under test is exercised by the deny cases.
func loopbackEgress(t *testing.T, srvURL string) *effects.EgressPolicy {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	return effects.NewEgressPolicy([]string{u.Hostname()}, true).InsecureAllowPrivateForTest()
}

// setFromPin builds a variable.set reading an effect node's output pin.
func setFromPin(id, name, from, fromPort string, next map[string]ExecTarget) *ExecNode {
	return varSet(id, name, []ExecDataInput{{Port: "value", From: from, FromPort: fromPort}}, next)
}

// TestEffects_HTTPRequestResumesContinuation: the full loop — park on
// a stamped wake key, worker fetch, intra-process ResumeExec, output
// pins bound in the task env, `then` chain continues.
func TestEffects_HTTPRequestResumesContinuation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":42}`))
	}))
	defer srv.Close()

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `"`)},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.out"},
					"error": {Node: "set.err"},
				}},
			"set.out": setFromPin("set.out", "out", "req", "body",
				map[string]ExecTarget{"then": {Node: "set.status"}}),
			"set.status": setFromPin("set.status", "status", "req", "status", nil),
			"set.err":    setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "req"}}},
	}
	eff := &SceneEffects{Runner: newTestRunner(t), Egress: loopbackEgress(t, srv.URL)}
	sc := effectsScene(t, "http-test", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.out", `{"answer":42}`, 2*time.Second)
	waitForState(t, sc, "__vars.bp.status", `200`, 2*time.Second)
	if v, _ := sc.state.Get("__vars.bp.err"); string(v) != `null` {
		t.Fatalf("error port fired on success: %s", v)
	}
}

// TestEffects_TwoInFlightResumeTheirOwnContinuations: two parked
// effects complete out of order; each wake key resumes ITS
// continuation with ITS bindings.
func TestEffects_TwoInFlightResumeTheirOwnContinuations(t *testing.T) {
	slowGate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("k") == "slow" {
			<-slowGate
			_, _ = w.Write([]byte(`"slow-value"`))
			return
		}
		_, _ = w.Write([]byte(`"fast-value"`))
	}))
	defer srv.Close()

	reqNode := func(id, k, set string) *ExecNode {
		return &ExecNode{ID: id, Op: OpHTTPRequest,
			Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `?k=` + k + `"`)},
			Next:   map[string]ExecTarget{"then": {Node: set}}}
	}
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"seq": {ID: "seq", Op: OpSequence, Next: map[string]ExecTarget{
				"then_0": {Node: "req.slow"},
				"then_1": {Node: "req.fast"},
			}},
			"req.slow": reqNode("req.slow", "slow", "set.slow"),
			"req.fast": reqNode("req.fast", "fast", "set.fast"),
			"set.slow": setFromPin("set.slow", "out", "req.slow", "body", nil),
			"set.fast": setFromPin("set.fast", "out2", "req.fast", "body", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "seq"}}},
	}
	eff := &SceneEffects{Runner: newTestRunner(t), Egress: loopbackEgress(t, srv.URL)}
	sc := effectsScene(t, "two-inflight", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	// The fast one completes first while the slow one is still parked —
	// UE latent semantics: the sequence forked both without waiting.
	waitForState(t, sc, "__vars.bp.out2", `"fast-value"`, 2*time.Second)
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		t.Fatalf("slow continuation resumed early: %s", v)
	}
	close(slowGate)
	waitForState(t, sc, "__vars.bp.out", `"slow-value"`, 2*time.Second)
}

// TestEffects_EgressDeniedToErrorPort: an unlisted host is denied by
// the policy — the chain resumes down `error` with the blocked reason,
// the denial is counted, nothing crashes, no task is killed.
func TestEffects_EgressDeniedToErrorPort(t *testing.T) {
	metrics := &fakeEffectMetrics{}
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{"url": raw(`"https://not-allowlisted.example.com/x"`)},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.out"},
					"error": {Node: "set.err"},
				}},
			"set.out": setFromPin("set.out", "out", "req", "body", nil),
			"set.err": setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "req"}}},
	}
	eff := &SceneEffects{
		Runner:  newTestRunner(t),
		Egress:  effects.NewEgressPolicy(nil, false), // deny-all
		Metrics: metrics,
	}
	sc := effectsScene(t, "egress-deny", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("__vars.bp.err"); ok && strings.Contains(string(v), "EGRESS_BLOCKED") {
			if metrics.blocked() == 0 {
				t.Fatal("denial must be counted on orion_http_egress_blocked_total")
			}
			if out, _ := sc.state.Get("__vars.bp.out"); string(out) != `null` {
				t.Fatalf("then port fired on denial: %s", out)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get("__vars.bp.err")
	t.Fatalf("error port did not fire, __vars.bp.err = %s", v)
}

// TestEffects_TimeoutToErrorPort_NoKill: the per-effect timeout is
// effect semantics — the error port fires; the SIBLING chain that ran
// alongside is untouched (no task kill).
func TestEffects_TimeoutToErrorPort_NoKill(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"seq": {ID: "seq", Op: OpSequence, Next: map[string]ExecTarget{
				"then_0": {Node: "req"},
				"then_1": {Node: "set.after"},
			}},
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{
					"url":        raw(`"` + srv.URL + `"`),
					"timeout_ms": raw(`50`),
				},
				Next: map[string]ExecTarget{"error": {Node: "set.err"}}},
			"set.after": varSet("set.after", "after", nil, nil),
			"set.err":   setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "seq"}}},
	}
	prog.Nodes["set.after"].Config["value"] = raw(`"ran"`)

	eff := &SceneEffects{Runner: newTestRunner(t), Egress: loopbackEgress(t, srv.URL)}
	sc := effectsScene(t, "timeout-test", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	// Sibling continues immediately (latent fork — never blocked).
	waitForState(t, sc, "__vars.bp.after", `"ran"`, 2*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("__vars.bp.err"); ok && strings.Contains(string(v), "EFFECT_TIMEOUT") {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get("__vars.bp.err")
	t.Fatalf("timeout must fire the error port, __vars.bp.err = %s", v)
}

// TestEffects_DBQueryTopologyA: the db.query op delegates to
// `${gateway}/<svc>/api/v1/_query` with the service token and binds
// rows/count/elapsed_ms — Orion side holds no DB anything.
func TestEffects_DBQueryTopologyA(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"rows":[{"n":"zab"}],"count":1,"elapsed_ms":3}`))
	}))
	defer srv.Close()

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"q": {ID: "q", Op: OpDBQuery,
				Config: map[string]json.RawMessage{
					"datasource": raw(`"truth"`),
					"descriptor": raw(`{"table":"players"}`),
				},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.rows"},
					"error": {Node: "set.err"},
				}},
			"set.rows": setFromPin("set.rows", "out", "q", "rows",
				map[string]ExecTarget{"then": {Node: "set.count"}}),
			"set.count": setFromPin("set.count", "status", "q", "count", nil),
			"set.err":   setFromPin("set.err", "err", "q", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "q"}}},
	}
	eff := &SceneEffects{
		Runner:      newTestRunner(t),
		DB:          effects.NewDBQueryClient(srv.URL, "orion-token", nil),
		DataSources: map[string]effects.DataSource{"truth": {Name: "truth", Svc: "truth"}},
	}
	sc := effectsScene(t, "dbq-test", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.out", `[{"n":"zab"}]`, 2*time.Second)
	waitForState(t, sc, "__vars.bp.status", `1`, 2*time.Second)
	if gotPath != "/truth/api/v1/_query" || gotAuth != "Bearer orion-token" {
		t.Fatalf("wire = %s auth=%q", gotPath, gotAuth)
	}
}

// TestEffects_DBQueryUndeclaredDataSource: the runtime backstop of the
// compile gate — an undeclared DataSource resolves to the error port
// with DATASOURCE_NOT_DECLARED (structural, db.query stays served).
func TestEffects_DBQueryUndeclaredDataSource(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"q": {ID: "q", Op: OpDBQuery,
				Config: map[string]json.RawMessage{"datasource": raw(`"rogue"`)},
				Next:   map[string]ExecTarget{"error": {Node: "set.err"}}},
			"set.err": setFromPin("set.err", "err", "q", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "q"}}},
	}
	eff := &SceneEffects{
		Runner:      newTestRunner(t),
		DB:          effects.NewDBQueryClient("http://zabgate.invalid", "tok", nil),
		DataSources: map[string]effects.DataSource{"truth": {Name: "truth", Svc: "truth"}},
	}
	sc := effectsScene(t, "dbq-undeclared", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("__vars.bp.err"); ok && strings.Contains(string(v), "DATASOURCE_NOT_DECLARED") {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get("__vars.bp.err")
	t.Fatalf("expected DATASOURCE_NOT_DECLARED on the error port, got %s", v)
}

// TestValidateExecDataSources: the structural compile-time gate.
func TestValidateExecDataSources(t *testing.T) {
	declared := map[string]effects.DataSource{"truth": {Name: "truth", Svc: "truth"}}
	ok := &ExecProgram{Nodes: map[string]*ExecNode{
		"q": {ID: "q", Op: OpDBQuery, Config: map[string]json.RawMessage{"datasource": raw(`"truth"`)}},
	}}
	if err := ValidateExecDataSources(ok, declared); err != nil {
		t.Fatalf("declared DataSource must validate: %v", err)
	}
	bad := &ExecProgram{Nodes: map[string]*ExecNode{
		"q": {ID: "q", Op: OpDBQuery, Config: map[string]json.RawMessage{"datasource": raw(`"rogue"`)}},
	}}
	err := ValidateExecDataSources(bad, declared)
	var verr *ExecValidationError
	if err == nil || !errorsAs(err, &verr) || verr.Code != "DATASOURCE_NOT_DECLARED" {
		t.Fatalf("err = %v, want DATASOURCE_NOT_DECLARED", err)
	}
	if err := ValidateExecDataSources(nil, declared); err != nil {
		t.Fatalf("nil program must validate: %v", err)
	}
}

// errorsAs avoids importing errors twice in this test file's context.
func errorsAs(err error, target **ExecValidationError) bool {
	v, ok := err.(*ExecValidationError)
	if ok {
		*target = v
	}
	return ok
}

// TestEffects_SourceReadDeclaredBinding: source.read fetches a
// DECLARED graph binding's URL on demand and binds `value`; an
// undeclared source name fails to the error port.
func TestEffects_SourceReadDeclaredBinding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"price":7}`))
	}))
	defer srv.Close()

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"rd": {ID: "rd", Op: OpSourceRead,
				Config: map[string]json.RawMessage{"source_id": raw(`"prices"`)},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.out"},
					"error": {Node: "set.err"},
				}},
			"rd.bad": {ID: "rd.bad", Op: OpSourceRead,
				Config: map[string]json.RawMessage{"source_id": raw(`"undeclared"`)},
				Next:   map[string]ExecTarget{"error": {Node: "set.err"}}},
			"set.out": setFromPin("set.out", "out", "rd", "value", nil),
			"set.err": setFromPin("set.err", "err", "rd.bad", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{
			"e1": {Target: ExecTarget{Node: "rd"}},
			"e2": {Target: ExecTarget{Node: "rd.bad"}},
		},
	}
	binding := compiler.ExternalAdapter{Kind: "http-poll", Key: "prices", URL: srv.URL}
	eff := &SceneEffects{Runner: newTestRunner(t)}
	sc := effectsScene(t, "source-test", prog, eff, binding)
	startScene(t, sc)

	mustFire(t, sc, "e1")
	waitForState(t, sc, "__vars.bp.out", `{"price":7}`, 2*time.Second)

	mustFire(t, sc, "e2")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("__vars.bp.err"); ok && strings.Contains(string(v), "SOURCE_NOT_DECLARED") {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get("__vars.bp.err")
	t.Fatalf("expected SOURCE_NOT_DECLARED on the error port, got %s", v)
}

// TestEffects_PoolRefusalToErrorPort: a stopped/full pool refuses the
// job — the effect resolves to EFFECT_QUEUE_FULL on the error port,
// synchronously, never an orphaned parked continuation.
func TestEffects_PoolRefusalToErrorPort(t *testing.T) {
	r := effects.NewRunner(1, 1, quietLogger())
	r.Start()
	r.Stop() // refuses every Submit from here on

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{"url": raw(`"https://api.example.com/x"`)},
				Next:   map[string]ExecTarget{"error": {Node: "set.err"}}},
			"set.err": setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "req"}}},
	}
	eff := &SceneEffects{Runner: r, Egress: effects.NewEgressPolicy([]string{"api.example.com"}, false)}
	sc := effectsScene(t, "pool-refusal", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("__vars.bp.err"); ok && strings.Contains(string(v), "EFFECT_QUEUE_FULL") {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get("__vars.bp.err")
	t.Fatalf("expected EFFECT_QUEUE_FULL on the error port, got %s", v)
}

// TestEffects_StaleCompletionDropped: a completion arriving after
// CancelExec carries the old epoch — dropped by the stamped wake-key
// gate, resumes nothing (ADR 003 §3.1.4).
func TestEffects_StaleCompletionDropped(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`"late"`))
	}))
	defer srv.Close()

	metrics := &fakeExecMetrics{}
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `"`)},
				Next:   map[string]ExecTarget{"then": {Node: "set.out"}}},
			"set.out": setFromPin("set.out", "out", "req", "body", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "req"}}},
	}
	eff := &SceneEffects{Runner: newTestRunner(t), Egress: loopbackEgress(t, srv.URL)}
	sc := effectsScene(t, "stale-completion", prog, eff)
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	mustFire(t, sc, "e")
	// Wait until the continuation is parked, then cancel (epoch bump).
	deadline := time.Now().Add(2 * time.Second)
	for metricsParked(metrics) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("continuation never parked")
		}
		time.Sleep(2 * time.Millisecond)
	}
	sc.CancelExec()
	close(release) // the worker now completes into a cancelled epoch

	deadline = time.Now().Add(3 * time.Second)
	for metrics.stale() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("stale completion was not dropped+counted")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		t.Fatalf("stale completion resumed a continuation: %s", v)
	}
}

func metricsParked(m *fakeExecMetrics) int {
	_, _, parked := m.counts()
	return parked
}

// TestEffects_TimeoutSeconds_NegZeroFallsBackToDefault: the IEEE-754
// `-0` trap — `-0 > 0` is false, so a `-0` authored timeout falls back
// to the default instead of becoming an instant (or infinite) timeout.
func TestEffects_TimeoutSeconds_NegZeroFallsBackToDefault(t *testing.T) {
	sc := effectsScene(t, "negzero", &ExecProgram{BlueprintKey: "bp", Nodes: map[string]*ExecNode{}, Entrypoints: map[string]ExecEntry{}}, &SceneEffects{})
	for _, tc := range []struct {
		name string
		cfg  string
		want time.Duration
	}{
		{"neg-zero", `-0`, defaultEffectTimeout},
		{"neg-zero-float", `-0.0`, defaultEffectTimeout},
		{"zero", `0`, defaultEffectTimeout},
		{"negative", `-5`, defaultEffectTimeout},
		{"absent", ``, defaultEffectTimeout},
		{"positive", `2.5`, 2500 * time.Millisecond},
	} {
		node := &ExecNode{ID: "n", Op: OpHTTPRequest, Config: map[string]json.RawMessage{}}
		if tc.cfg != "" {
			node.Config["timeout_seconds"] = raw(tc.cfg)
		}
		got := sc.effectTimeout(&execTask{env: map[string]json.RawMessage{}}, node)
		if got != tc.want {
			t.Errorf("%s: timeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEffects_CompletionTravelsIntraProcess pins the delivery design:
// the completion is an InputMsg{ResumeExec, ResumeEnv} — it carries NO
// state path, so it can never be confused with (or forged as) a
// `__system.*` state write. A plain `__system.*` system write into the
// same scene resumes nothing.
func TestEffects_CompletionTravelsIntraProcess(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"lat": {ID: "lat", Op: "test.latent",
				Next: map[string]ExecTarget{"then": {Node: "set.out"}}},
			"set.out": varSet("set.out", "out", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "lat"}}},
	}
	prog.Nodes["set.out"].Config["value"] = raw(`"resumed"`)

	sc := effectsScene(t, "intra-process", prog, &SceneEffects{Runner: newTestRunner(t)})
	sc.registerExecOp("test.latent", func(_ *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
		tgt := node.Next["then"]
		return execOpOutcome{park: true, parkKey: "wk|sha256:effects-test|0|999", resume: tgt}
	})
	startScene(t, sc)
	mustFire(t, sc, "e")

	// A free __system.* write (even system-marked) resumes nothing —
	// only a ResumeExec message can.
	sc.Input(InputMsg{Path: "__system.anim.report", Value: raw(`{"wake_key":"wk|sha256:effects-test|0|999"}`), Source: "system:forged", IsSystem: true})
	time.Sleep(50 * time.Millisecond)
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		t.Fatalf("a __system.* write resumed a continuation: %s", v)
	}

	sc.Input(InputMsg{ResumeExec: "wk|sha256:effects-test|0|999", Source: "system:effect/lat"})
	waitForState(t, sc, "__vars.bp.out", `"resumed"`, 2*time.Second)
}
