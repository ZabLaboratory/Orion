package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Operator route addressing for a LEGACY single-blueprint scene (Blue ADR 008
// §3.2, ADR 016 RC-6, e2e #152 gap). The canvas-chat-sponso scene pushes
// legacy-singular: its layout binds non-keyed pl.*/chat.* leaves, so the
// blueprint rides the EMPTY scene-local key "". An empty key cannot ride a
// path segment (Go ServeMux never matches `{blueprint_id}` empty), so the
// cockpit's `blueprint_id:""` produced an unroutable `POST /operator/call//…`
// (404) — the real cockpit button could not fire the on-call, although
// FireOnCall worked. The fix: the cockpit announces "_" for the empty key and
// the operator routes decode "_" back to "" (resolveBlueprintKey). Same route,
// same contract, local==antenne. These tests exercise the REAL HTTP router.

const legacySceneID = "33333333-3333-3333-3333-333333333333"

// newLegacyFixture builds a Show whose active scene hosts the DEFAULT (empty
// key "") blueprint with an on-call entrypoint "on_lck" — exactly the
// canvas-chat-sponso shape.
func newLegacyFixture(t *testing.T) *operatorFixture {
	t.Helper()
	m := obs.NewMetrics()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	show.SetExecMetrics(m)
	t.Cleanup(show.Stop)

	graph := &compiler.Graph{
		SceneID: legacySceneID, SceneVersion: "sha256:legacy",
		Defaults: map[string]json.RawMessage{"__vars..called": json.RawMessage(`null`)},
	}
	// Empty BlueprintKey "" — the legacy/default key. on_lck → variable.set.
	prog := &runtime.ExecProgram{
		BlueprintKey: "",
		Nodes: map[string]*runtime.ExecNode{
			"set.called": {ID: "set.called", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{"variable": json.RawMessage(`"called"`)},
				Data:   []runtime.ExecDataInput{{Port: "value", From: "on_lck", FromPort: "payload"}}},
		},
		Entrypoints: map[string]runtime.ExecEntry{
			"on_lck": {Kind: runtime.EntryOnCall, Node: "on_lck", Target: runtime.ExecTarget{Node: "set.called"}},
		},
	}
	show.LoadExec(legacySceneID, graph, &compiler.RenderBundle{SceneVersion: "sha256:legacy"}, prog)
	if err := show.SetActive(legacySceneID, nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: testLogger(), Metrics: m, Show: show})
	return &operatorFixture{mux: mux, show: show}
}

func (f *operatorFixture) waitLegacyVar(t *testing.T, leaf, want string) {
	t.Helper()
	sc, err := f.show.Get(legacySceneID)
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

// TestLegacy_EmptyKeyPathDoesNotFire pins the DEFECT: the empty-segment URL the
// old cockpit built (`blueprint_id:""`) is unroutable — Go's ServeMux never
// matches an empty `{blueprint_id}` segment, so the request never reaches the
// handler and the on-call never fires. (ServeMux collapses the `//` and emits a
// 307 to a non-matching path; the operative fact is "not 202".) Guards against
// a "fix" that quietly accepts an empty segment instead of the token.
func TestLegacy_EmptyKeyPathDoesNotFire(t *testing.T) {
	f := newLegacyFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call//on_lck", "operator",
		map[string]any{"payload": 1})
	if w.Code == http.StatusAccepted {
		t.Fatalf("empty blueprint segment must NOT fire the on-call; got %d (the defect was a non-202 unroutable URL)", w.Code)
	}
}

// TestLegacy_DefaultTokenFiresOnCall is THE proof: POST /operator/call/_/on_lck
// on a legacy active scene → 202 and the spine fires, over the real HTTP route
// (not FireOnCall directly).
func TestLegacy_DefaultTokenFiresOnCall(t *testing.T) {
	f := newLegacyFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/on_lck", "operator",
		map[string]any{"payload": map[string]any{"region": "LCK"}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("default-token call: got %d, want 202 (body=%s)", w.Code, w.Body.String())
	}
	f.waitLegacyVar(t, "__vars..called", `{"region":"LCK"}`)
}

// TestLegacy_DefaultTokenUnknownEntrypointIs409 confirms the token resolves to
// the active default blueprint (a wrong entrypoint is 409, not 404) — i.e. the
// route reached the handler against the empty-key program.
func TestLegacy_DefaultTokenUnknownEntrypointIs409(t *testing.T) {
	f := newLegacyFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/call/_/nope", "operator",
		map[string]any{"payload": 1})
	if w.Code != http.StatusConflict {
		t.Fatalf("unknown entrypoint on default blueprint: got %d, want 409 (body=%s)",
			w.Code, w.Body.String())
	}
}

// TestLegacy_CockpitAnnouncesDefaultToken proves the contract advertises an
// ADDRESSABLE blueprint_id ("_"), not "" — so the cockpit builds a routable URL.
func TestLegacy_CockpitAnnouncesDefaultToken(t *testing.T) {
	f := newLegacyFixture(t)
	w := opRequest(t, f.mux, "GET", "/api/v1/cockpit/contracts?stream_id=s1", "operator", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cockpit: got %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var resp cockpitContracts
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, trig := range resp.Triggers {
		if trig.EntrypointID == "on_lck" {
			found = true
			if trig.BlueprintKey != defaultBlueprintToken {
				t.Fatalf("trigger blueprint_id = %q, want %q (the addressable default token)",
					trig.BlueprintKey, defaultBlueprintToken)
			}
		}
	}
	if !found {
		t.Fatalf("on_lck trigger not announced; triggers=%+v", resp.Triggers)
	}
	// The announced token must round-trip through the call route (the URL the
	// cockpit constructs from the contract).
	call := opRequest(t, f.mux, "POST",
		"/api/v1/operator/call/"+defaultBlueprintToken+"/on_lck", "operator",
		map[string]any{"payload": 1})
	if call.Code != http.StatusAccepted {
		t.Fatalf("announced-token call: got %d, want 202 (body=%s)", call.Code, call.Body.String())
	}
}

// TestBlueprintKey_RoundTrip pins the exact encode/decode inverse so the cockpit
// announcement and the operator decode can never drift.
func TestBlueprintKey_RoundTrip(t *testing.T) {
	cases := []string{"", "bp", "left", "right"}
	for _, key := range cases {
		if got := resolveBlueprintKey(addressBlueprintKey(key)); got != key {
			t.Fatalf("round-trip %q → %q → %q (not identity)",
				key, addressBlueprintKey(key), got)
		}
	}
	// The empty key is the ONLY one aliased to the token.
	if addressBlueprintKey("") != defaultBlueprintToken {
		t.Fatal("empty key must encode to the default token")
	}
	if resolveBlueprintKey(defaultBlueprintToken) != "" {
		t.Fatal("default token must decode to the empty key")
	}
	// A keyed (non-default) blueprint is untouched both ways.
	if addressBlueprintKey("bp") != "bp" || resolveBlueprintKey("bp") != "bp" {
		t.Fatal("non-default key must pass through verbatim")
	}
}
