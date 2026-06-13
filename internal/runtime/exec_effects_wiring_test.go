package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// Tests for the R9 activation WIRING (ADR 006 §3.4, M2). The executor
// behaviour is proven in exec_effects_test.go; these prove the SEAM that
// turns it on in production: Show.SetEffects + LoadExec installs the
// world-effect ops ONLY when a scene loads with a non-empty (validated,
// exec-bearing) program set — and a non-validated / pure-dataflow scene
// (empty progs) NEVER registers them, so it can never open a socket or a
// query (the load-bearing safety claim of the R9 lift).

// dbOnStartProg fires `on-start` → db.query(datasource=truth) →
// variable.set(out = rows). The simplest "real effect at activation" the
// wiring must arm for a validated scene and deny for a dataflow one.
func dbOnStartProg() *ExecProgram {
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"q": {ID: "q", Op: OpDBQuery,
				Config: map[string]json.RawMessage{
					"datasource": raw(`"truth"`),
					"descriptor": raw(`{"table":"players"}`),
				},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.out"},
					"error": {Node: "set.err"},
				}},
			"set.out": setFromPin("set.out", "out", "q", "rows", nil),
			"set.err": setFromPin("set.err", "err", "q", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Target: ExecTarget{Node: "q"}, Kind: EntryOnStart, Node: "startn"},
		},
	}
}

// wiringBundle builds the prod-shaped bundle the Show installs: a real
// worker pool, a deny-all egress policy (the http.request anti-SSRF
// surface), and a topology-A db.query client pointed at the stub gateway.
func wiringBundle(t *testing.T, gatewayURL, token string) *SceneEffects {
	t.Helper()
	return &SceneEffects{
		Runner:      newTestRunner(t),
		Egress:      effects.NewEgressPolicy(nil, false), // deny-all (fail-closed)
		DB:          effects.NewDBQueryClient(gatewayURL, token, nil),
		DataSources: map[string]effects.DataSource{"truth": {Name: "truth", Svc: "truth"}},
	}
}

// TestWiring_ValidatedSceneArmsEffectsThroughShow: a scene loaded with a
// NON-EMPTY program set (the execForAir contract for a validated,
// exec-bearing version) gets the world ops installed by the Show; its
// on-start db.query reaches the stub `_query` and its rows land on a
// leaf. This is the positive half of the R9 seam — db.query topology A,
// driven end-to-end through Show.SetEffects + LoadExec.
func TestWiring_ValidatedSceneArmsEffectsThroughShow(t *testing.T) {
	var gotPath, gotAuth atomic.Value
	gotPath.Store("")
	gotAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"rows":[{"team":"zab"}],"count":1,"elapsed_ms":2}`))
	}))
	defer srv.Close()

	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.SetEffects(wiringBundle(t, srv.URL, "orion-svc-token"))

	graph := effectsGraph("scene-validated")
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:effects-test"}
	// A non-empty program set is exactly what execForAir returns for a
	// validated exec-bearing version — the seam keys on len(progs) > 0.
	show.LoadExec("scene-validated", graph, bundle, dbOnStartProg())

	sc, err := show.Get("scene-validated")
	if err != nil {
		t.Fatal(err)
	}
	// The world ops MUST be registered (the validated path). source.read is
	// no longer a world op (ADR 012 Option B — pure compute), so only
	// db.query / http.request remain in the SetEffects registration set.
	for _, op := range []string{OpDBQuery, OpHTTPRequest} {
		if _, ok := sc.execOps[op]; !ok {
			t.Fatalf("validated scene missing world op %q in registry", op)
		}
	}

	// Activation fires on-start → db.query → leaf. SetActive seeds on air
	// and fires on-start on the live instance.
	if err := show.SetActive("scene-validated", nil); err != nil {
		t.Fatal(err)
	}
	waitForState(t, sc, "__vars.bp.out", `[{"team":"zab"}]`, 2*time.Second)
	if p := gotPath.Load().(string); p != "/truth/api/v1/_query" {
		t.Fatalf("db.query hit wrong wire path: %q", p)
	}
	if a := gotAuth.Load().(string); a != "Bearer orion-svc-token" {
		t.Fatalf("db.query missing service-token bearer: %q", a)
	}
}

// TestWiring_DataflowSceneRegistersNoWorldOps (R9, load-bearing): a scene
// loaded with an EMPTY program set — what execForAir returns for a
// NON-VALIDATED or pure-dataflow version — must register ZERO
// world-effect ops. The seam never calls SetEffects, so http.request /
// db.query are not in the registry: an authored occurrence halts-at-node
// with NO egress and NO query. This is the negative assertion that an
// unvalidated scene can never touch the world. (source.read is no longer a
// world op — ADR 012 Option B reclassified it to a pure compute.)
func TestWiring_DataflowSceneRegistersNoWorldOps(t *testing.T) {
	var gatewayHit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gatewayHit.Store(true)
		_, _ = w.Write([]byte(`{"rows":[],"count":0,"elapsed_ms":0}`))
	}))
	defer srv.Close()

	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	// The bundle IS configured on the show — proving the gate is the
	// program set, not a missing bundle.
	show.SetEffects(wiringBundle(t, srv.URL, "orion-svc-token"))

	graph := effectsGraph("scene-dataflow")
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:effects-test"}
	// Empty program set: a non-validated / pure-dataflow version.
	show.LoadExec("scene-dataflow", graph, bundle)

	sc, err := show.Get("scene-dataflow")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{OpDBQuery, OpHTTPRequest} {
		if _, ok := sc.execOps[op]; ok {
			t.Fatalf("R9 VIOLATION: dataflow scene registered world op %q — an unvalidated scene must touch nothing", op)
		}
	}
	// sh.effects on the scene must be nil — SetEffects was never called.
	if sc.effects != nil {
		t.Fatal("R9 VIOLATION: dataflow scene carries a SceneEffects bundle")
	}

	// Activate it and let any backstage timers settle: even live, no
	// gateway query can fire (the op simply does not exist on this scene).
	if err := show.SetActive("scene-dataflow", nil); err != nil {
		t.Fatal(err)
	}
	drainScene(t, sc)
	time.Sleep(50 * time.Millisecond)
	if gatewayHit.Load() {
		t.Fatal("R9 VIOLATION: a non-validated scene reached the _query gateway")
	}
}

// TestWiring_HTTPRequestAntiSSRFThroughShow: the egress policy the Show
// installs is fail-closed — an http.request to a non-allowlisted host on
// a validated scene resolves to the error port (EGRESS_BLOCKED), never a
// socket to an internal address. Proves the anti-SSRF surface is wired,
// not just unit-tested in isolation.
func TestWiring_HTTPRequestAntiSSRFThroughShow(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{
					"url": raw(`"http://169.254.169.254/latest/meta-data/"`),
				},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.out"},
					"error": {Node: "set.err"},
				}},
			"set.out": setFromPin("set.out", "out", "req", "body", nil),
			"set.err": setFromPin("set.err", "err", "req", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Target: ExecTarget{Node: "req"}, Kind: EntryOnStart, Node: "startn"},
		},
	}

	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.SetEffects(wiringBundle(t, "http://unused.invalid", "tok"))

	graph := effectsGraph("scene-ssrf")
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:effects-test"}
	show.LoadExec("scene-ssrf", graph, bundle, prog)
	sc, err := show.Get("scene-ssrf")
	if err != nil {
		t.Fatal(err)
	}
	if err := show.SetActive("scene-ssrf", nil); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("__vars.bp.err"); ok && strings.Contains(string(v), "EGRESS_BLOCKED") {
			if out, _ := sc.state.Get("__vars.bp.out"); string(out) != `null` {
				t.Fatalf("then port fired on a blocked egress: %s", out)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get("__vars.bp.err")
	t.Fatalf("anti-SSRF egress denial did not reach the error port, __vars.bp.err = %s", v)
}
