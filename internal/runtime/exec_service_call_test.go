package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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
