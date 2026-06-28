package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/effects"
)

// recordingMinter records the token-paths it is asked to scope and returns
// a fixed bearer — the seam that proves RC #4 (token minted with the
// route's token_paths only).
type recordingMinter struct {
	mu    sync.Mutex
	asked [][]string
	token string
}

func (m *recordingMinter) mint(paths []string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, append([]string(nil), paths...))
	return m.token
}

func (m *recordingMinter) lastAsked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.asked) == 0 {
		return nil
	}
	return m.asked[len(m.asked)-1]
}

const echoRouteJSON = `{"service":"example","route_id":"example.echo","method":"POST",` +
	`"path_template":"/example/api/v1/items/{name}/echo","params":["name"],"token_paths":["example.echo"]}`

func serviceCallProgram(routeJSON, paramsJSON string) *ExecProgram {
	call := &ExecNode{
		ID: "call", Op: OpServiceCall,
		Config: map[string]json.RawMessage{
			bakedRouteConfigKey: raw(routeJSON),
			"params":            raw(paramsJSON),
		},
		Next: map[string]ExecTarget{
			"then":  {Node: "set.status"},
			"error": {Node: "set.err"},
		},
	}
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"call":       call,
			"set.status": setFromPin("set.status", "status", "call", "status", nil),
			"set.err":    setFromPin("set.err", "err", "call", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "call"}}},
	}
}

// TestServiceCall_BuildsPathAndCalls is the conformance-named proof: the
// runtime constructs the path from the baked template, mints a token scoped
// to the route's token_paths, POSTs through the gateway with that token (no
// caller Authorization), and binds status on `then`.
func TestServiceCall_BuildsPathAndCalls(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	minter := &recordingMinter{token: "scoped-bearer-xyz"}
	eff := &SceneEffects{
		Runner:      newTestRunner(t),
		ServiceCall: effects.NewServiceCallClient(srv.URL, minter.mint, nil),
	}
	prog := serviceCallProgram(echoRouteJSON, `{"name":"alice"}`)
	sc := effectsScene(t, "svc-call", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.status", `200`, 2*time.Second)

	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	// Path built by the runtime from the curated template + escaped param.
	if gotPath != "/example/api/v1/items/alice/echo" {
		t.Errorf("path = %q, want /example/api/v1/items/alice/echo", gotPath)
	}
	// The scoped service token rode — NOT any caller Authorization (none
	// exists on this path; the client never reads/forwards caller auth).
	if gotAuth != "Bearer scoped-bearer-xyz" {
		t.Errorf("authorization = %q, want the scoped bearer", gotAuth)
	}
	// RC #4: minted with EXACTLY the route's token_paths.
	asked := minter.lastAsked()
	if len(asked) != 1 || asked[0] != "example.echo" {
		t.Errorf("minted token_paths = %v, want [example.echo]", asked)
	}
}

// TestServiceCall_AntiInjectionTemplate: an authored param carrying path
// separators / traversal cannot break out of its segment — the runtime
// escapes it, the only structural slashes are the template's own.
func TestServiceCall_AntiInjectionTemplate(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	minter := &recordingMinter{token: "tok"}
	eff := &SceneEffects{
		Runner:      newTestRunner(t),
		ServiceCall: effects.NewServiceCallClient(srv.URL, minter.mint, nil),
	}
	prog := serviceCallProgram(echoRouteJSON, `{"name":"../../admin/disable"}`)
	sc := effectsScene(t, "svc-inject", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.status", `200`, 2*time.Second)

	want := "/example/api/v1/items/..%2F..%2Fadmin%2Fdisable/echo"
	if gotPath != want {
		t.Errorf("escaped path = %q, want %q (param must stay one segment)", gotPath, want)
	}
}

// TestServiceCall_MissingBakedRouteFailsClosed: a node reaching the runtime
// without a baked route (compiler backstop) routes to `error`, never the
// world.
func TestServiceCall_MissingBakedRouteFailsClosed(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	minter := &recordingMinter{token: "tok"}
	eff := &SceneEffects{
		Runner:      newTestRunner(t),
		ServiceCall: effects.NewServiceCallClient(srv.URL, minter.mint, nil),
	}
	// No __route config.
	call := &ExecNode{ID: "call", Op: OpServiceCall,
		Next: map[string]ExecTarget{"error": {Node: "set.err"}}}
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"call":    call,
			"set.err": setFromPin("set.err", "err", "call", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "call"}}},
	}
	sc := effectsScene(t, "svc-nobake", prog, eff)
	startScene(t, sc)
	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.err", `"EGRESS_ROUTE_NOT_BAKED"`, 2*time.Second)
	if called {
		t.Error("server was called despite a missing baked route — must fail closed")
	}
}

// countingServiceServer returns an httptest server that counts hits and a
// pointer to the counter — the seam the budget tests use to prove the
// egress request was (not) emitted.
func countingServiceServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestServiceCall_BudgetExceededFailsClosed: the per-stream egress budget
// (ADR Blue 009 §B / R3) caps service.call on a stream. A burst beyond the
// budget is rejected on the node's `error` port (EGRESS_BUDGET_EXCEEDED),
// the downstream service is NEVER hit past the bound, and the metric ticks
// — no crash, no blocked tick. (Issue #265 RC: burst → cut at threshold.)
func TestServiceCall_BudgetExceededFailsClosed(t *testing.T) {
	srv, hits := countingServiceServer(t)

	minter := &recordingMinter{token: "tok"}
	metrics := &fakeEffectMetrics{}
	eff := &SceneEffects{
		Runner:       newTestRunner(t),
		ServiceCall:  effects.NewServiceCallClient(srv.URL, minter.mint, nil),
		EgressBudget: effects.NewStreamEgressLimiter(2, 10), // 2 calls / 10 s
		Metrics:      metrics,
	}
	prog := serviceCallProgram(echoRouteJSON, `{"name":"alice"}`)
	sc := effectsScene(t, "svc-budget", prog, eff) // default "live" stream key
	startScene(t, sc)

	// Three fires; the first two spend the budget, the third is denied.
	mustFire(t, sc, "e")
	mustFire(t, sc, "e")
	mustFire(t, sc, "e")

	// The denial binds synchronously; await it, then let the two allowed
	// calls settle on the server.
	waitForState(t, sc, "__vars.bp.err", `"EGRESS_BUDGET_EXCEEDED"`, 2*time.Second)
	waitFor(t, "two allowed calls reach the server", func() bool { return hits.Load() == 2 })

	// Settle window: the server is NEVER hit past the budget of 2.
	time.Sleep(50 * time.Millisecond)
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want exactly 2 (budget cap)", got)
	}
	if got := metrics.budgetExc(); got != 1 {
		t.Fatalf("budget-exceeded metric = %d, want 1", got)
	}
}

// TestServiceCall_BudgetIsolatedPerStream: a stream saturating its budget
// does NOT starve another stream — each meters independently (issue #265
// RC: one stream saturated, another unaffected). Both scenes share ONE
// effects bundle (and one limiter), differing only by stream key.
func TestServiceCall_BudgetIsolatedPerStream(t *testing.T) {
	srv, hits := countingServiceServer(t)

	minter := &recordingMinter{token: "tok"}
	eff := &SceneEffects{
		Runner:       newTestRunner(t),
		ServiceCall:  effects.NewServiceCallClient(srv.URL, minter.mint, nil),
		EgressBudget: effects.NewStreamEgressLimiter(1, 10), // 1 call / 10 s
	}

	// Stream A (default "live"): one allowed call, the second denied.
	progA := serviceCallProgram(echoRouteJSON, `{"name":"a"}`)
	scA := effectsScene(t, "svc-iso-a", progA, eff)
	startScene(t, scA)
	mustFire(t, scA, "e")
	mustFire(t, scA, "e")
	waitForState(t, scA, "__vars.bp.err", `"EGRESS_BUDGET_EXCEEDED"`, 2*time.Second)
	waitFor(t, "stream A's one allowed call reaches the server", func() bool { return hits.Load() == 1 })

	// Stream B (distinct key): its budget is untouched by A's saturation —
	// the call goes through.
	progB := serviceCallProgram(echoRouteJSON, `{"name":"b"}`)
	scB := effectsScene(t, "svc-iso-b", progB, eff)
	scB.SetStreamKey("other-stream")
	startScene(t, scB)
	mustFire(t, scB, "e")
	waitForState(t, scB, "__vars.bp.status", `200`, 2*time.Second)

	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (one per stream — B unaffected by A)", got)
	}
}
