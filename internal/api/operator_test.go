package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Operator runtime surface tests (Orion #209, Blue ADR 008 §3.2/§3.3) at
// the HTTP boundary: authz, active-only 409, on-call fire, pending listing,
// resolve type-check, and the invariant-7 late-resolve 410.

const opSceneID = "11111111-1111-1111-1111-111111111111"

// operatorFixture builds a Show with one scene hosting blueprint key "bp"
// with an on-call entrypoint ("call") and an await-value node ("pick"). The
// scene is made active so the operator routes (active-only) reach it.
type operatorFixture struct {
	mux  *http.ServeMux
	show *runtime.Show
}

func newOperatorFixture(t *testing.T) *operatorFixture {
	t.Helper()
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	show.SetExecMetrics(m)
	t.Cleanup(show.Stop)

	graph := &compiler.Graph{
		SceneID: opSceneID, SceneVersion: "sha256:op",
		Defaults: map[string]json.RawMessage{"__vars.bp.called": json.RawMessage(`null`)},
	}
	// on-call → variable.set(called = payload). await → variable.set(picked = value).
	prog := &runtime.ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*runtime.ExecNode{
			"set.called": {ID: "set.called", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{"variable": json.RawMessage(`"called"`)},
				Data:   []runtime.ExecDataInput{{Port: "value", From: "call", FromPort: "payload"}}},
			"pick": {ID: "pick", Op: runtime.OpOperatorAwait,
				Config: map[string]json.RawMessage{
					"await_name": json.RawMessage(`"pick"`),
					"value_type": json.RawMessage(`"core.primitive.integer"`)},
				Next: map[string]runtime.ExecTarget{"then": {Node: "set.picked"}}},
			"set.picked": {ID: "set.picked", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{"variable": json.RawMessage(`"picked"`)},
				Data:   []runtime.ExecDataInput{{Port: "value", From: "pick", FromPort: "value"}}},
		},
		Entrypoints: map[string]runtime.ExecEntry{
			"call": {Kind: runtime.EntryOnCall, Node: "call", Target: runtime.ExecTarget{Node: "set.called"}},
			// on-start arms the await as soon as the scene goes live.
			"arm": {Kind: runtime.EntryOnStart, Target: runtime.ExecTarget{Node: "pick"}},
		},
	}
	show.LoadExec(opSceneID, graph, &compiler.RenderBundle{SceneVersion: "sha256:op"}, prog)
	if err := show.SetActive(opSceneID, nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: testLogger(), Metrics: m, Show: show})
	return &operatorFixture{mux: mux, show: show}
}

func opRequest(t *testing.T, mux *http.ServeMux, method, path, role string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, &buf)
	if role != "" {
		r.Header.Set("X-Authenticated-User", "op-user")
		r.Header.Set("X-Authenticated-Role", role)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func (f *operatorFixture) waitVar(t *testing.T, leaf, want string) {
	t.Helper()
	sc, err := f.show.Get(opSceneID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sub, snap := sc.Subscribe(16)
		v, ok := snap.State[leaf]
		sub.Close()
		if ok && string(v) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("leaf %s never reached %s", leaf, want)
}

func TestOperator_CallRequiresOperatorRole(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/bp/call", "viewer",
		map[string]any{"payload": 1})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer call: got %d, want 403", w.Code)
	}
}

func TestOperator_CallDormantBlueprintIs409(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/other-bp/call", "operator",
		map[string]any{"payload": 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("dormant blueprint call: got %d, want 409", w.Code)
	}
}

func TestOperator_CallUnknownEntrypointIs409(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/bp/nope", "operator",
		map[string]any{"payload": 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("unknown entrypoint: got %d, want 409", w.Code)
	}
}

func TestOperator_CallFiresWithPayload(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/bp/call", "operator",
		map[string]any{"payload": map[string]any{"x": 7}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("call: got %d, want 202 (body=%s)", w.Code, w.Body.String())
	}
	f.waitVar(t, "__vars.bp.called", `{"x":7}`)
}

func TestOperator_PendingListsArmedAwait(t *testing.T) {
	f := newOperatorFixture(t)
	// The on-start arm parks the await; poll the pending route until listed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w := opRequest(t, f.mux, "GET", "/api/v1/runtime/bp/pending", "operator", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("pending: got %d, want 200", w.Code)
		}
		var resp struct {
			Pending []runtime.PendingAwait `json:"pending"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Pending) == 1 && resp.Pending[0].AwaitName == "pick" {
			if resp.Pending[0].ValueType != "core.primitive.integer" {
				t.Fatalf("value_type = %q", resp.Pending[0].ValueType)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("await never listed in pending")
}

func TestOperator_ResolveTypeMismatchIs422(t *testing.T) {
	f := newOperatorFixture(t)
	waitAwaitArmed(t, f)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/bp/pick", "operator",
		map[string]any{"value": 3.5})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("type mismatch: got %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
}

func TestOperator_ResolveValidResumes(t *testing.T) {
	f := newOperatorFixture(t)
	waitAwaitArmed(t, f)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/bp/pick", "operator",
		map[string]any{"value": 42})
	if w.Code != http.StatusOK {
		t.Fatalf("resolve: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	f.waitVar(t, "__vars.bp.picked", `42`)
}

func TestOperator_LateResolveAfterSwitchAwayIs410(t *testing.T) {
	f := newOperatorFixture(t)
	waitAwaitArmed(t, f)
	// Switch away: load + activate a second scene; the await of the first is
	// invalidated (ADR 008 invariant 7).
	const other = "22222222-2222-2222-2222-222222222222"
	f.show.LoadExec(other,
		&compiler.Graph{SceneID: other, SceneVersion: "sha256:op2"},
		&compiler.RenderBundle{SceneVersion: "sha256:op2"})
	if err := f.show.SetActive(other, nil); err != nil {
		t.Fatalf("SetActive other: %v", err)
	}
	// bp is no longer active → 410 (dormant blueprint).
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/bp/pick", "operator",
		map[string]any{"value": 1})
	if w.Code != http.StatusGone {
		t.Fatalf("late resolve after switch-away: got %d, want 410 (body=%s)", w.Code, w.Body.String())
	}
}

// waitAwaitArmed polls until the await is registered (on-start armed it).
func waitAwaitArmed(t *testing.T, f *operatorFixture) {
	t.Helper()
	sc, err := f.show.Get(opSceneID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sc.PendingAwaits("bp")) == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("await never armed")
}
