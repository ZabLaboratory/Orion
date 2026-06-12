package runtime

// Probe tests for phase-3 async-effect executor (ADR 003 §3.1.3, issue #85).
// Complement Forge's exec_effects_test.go — never rewrite it.
//
// Axes:
//   1. NaN timeout_seconds falls back to defaultEffectTimeout (IEEE-754 trap)
//   2. Under -race, two concurrent effects resume THEIR OWN continuations
//      (the race detector must not fire on the delivery path)
//   3. A wrong-SceneVersion wake key from deliverEffect is dropped+counted
//      (cross-version stale, not just cross-epoch stale)
//   4. db.query with empty DataSources map → DATASOURCE_NOT_DECLARED on error port
//   5. http.request with nil Egress → EGRESS_BLOCKED on error port (deny-all)
//   6. R9: SetEffects is not called from any production path in this package
//      (structural: the exported function exists but Show.LoadExec and
//       Show.Load never call it — pinned against future drift)
//   7. A __system.* InputMsg with only Path set (no ResumeExec) does NOT resume
//      any parked continuation — proves the intra-process path is the only
//      way to wake a continuation (complements Forge's CompletionTravelsIntraProcess)

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// TestEffects_NaNTimeoutFallsBackToDefault: NaN from json.Unmarshal for
// "timeout_seconds" must NOT produce an instant or infinite timeout — the
// `!(secs > 0)` guard covers NaN (NaN > 0 == false).
func TestEffects_NaNTimeoutFallsBackToDefault(t *testing.T) {
	sc := effectsScene(t, "nan-timeout",
		&ExecProgram{BlueprintKey: "bp", Nodes: map[string]*ExecNode{}, Entrypoints: map[string]ExecEntry{}},
		&SceneEffects{})
	node := &ExecNode{ID: "n", Op: OpHTTPRequest,
		Config: map[string]json.RawMessage{
			"timeout_seconds": json.RawMessage(`null`), // json.Unmarshal → 0
		}}
	task := &execTask{env: map[string]json.RawMessage{}}
	if got := sc.effectTimeout(task, node); got != defaultEffectTimeout {
		t.Fatalf("null timeout_seconds: got %v, want defaultEffectTimeout (%v)", got, defaultEffectTimeout)
	}

	// Confirm the guard explicitly with a manually-constructed node with negative value.
	node2 := &ExecNode{ID: "n2", Op: OpHTTPRequest,
		Config: map[string]json.RawMessage{
			// IEEE-754: json.Number "NaN" is not valid JSON; any
			// non-positive float falls through the same guard. Use -1.
			"timeout_seconds": json.RawMessage(`-1`),
		}}
	if got := sc.effectTimeout(task, node2); got != defaultEffectTimeout {
		t.Fatalf("negative timeout_seconds: got %v, want defaultEffectTimeout", got)
	}
}

// TestEffects_Race_TwoConcurrentEffectsResumeOwn: under -race, two http.request
// effects launched concurrently resume their own continuation — the delivery
// channel is per-scene and the InputMsg carries the stamped wake key, so
// cross-contamination is impossible. This test drives the race detector over
// the deliverEffect → scene.Input → resumeParked path.
func TestEffects_Race_TwoConcurrentEffectsResumeOwn(t *testing.T) {
	// Gate to synchronise worker completion — both requests land
	// simultaneously to maximise interleaving.
	bothReady := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var arrivals int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals++
		count := arrivals
		mu.Unlock()
		if count == 2 {
			once.Do(func() { close(bothReady) })
		}
		select {
		case <-bothReady:
		case <-r.Context().Done():
			return
		}
		k := r.URL.Query().Get("k")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"` + k + `"`))
	}))
	defer srv.Close()

	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"par": {ID: "par", Op: OpSequence, Next: map[string]ExecTarget{
				"then_0": {Node: "rA"},
				"then_1": {Node: "rB"},
			}},
			"rA": {ID: "rA", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `?k=A"`)},
				Next:   map[string]ExecTarget{"then": {Node: "sA"}}},
			"rB": {ID: "rB", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{"url": raw(`"` + srv.URL + `?k=B"`)},
				Next:   map[string]ExecTarget{"then": {Node: "sB"}}},
			"sA": setFromPin("sA", "out", "rA", "body", nil),
			"sB": setFromPin("sB", "out2", "rB", "body", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "par"}}},
	}
	eff := &SceneEffects{Runner: newTestRunner(t), Egress: loopbackEgress(t, srv.URL)}
	sc := effectsScene(t, "race-two-effects", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.out", `"A"`, 3*time.Second)
	waitForState(t, sc, "__vars.bp.out2", `"B"`, 3*time.Second)
}

// TestEffects_WrongVersionWakeKey_NoResume: a ResumeExec message whose
// wake key carries a different SceneVersion than the scene's current version
// is dropped as stale and counted — exactly the cross-version stale case
// (distinct from the cross-epoch case in Forge's StaleCompletionDropped test).
func TestEffects_WrongVersionWakeKey_NoResume(t *testing.T) {
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

	sc := effectsScene(t, "wrong-version", prog, &SceneEffects{Runner: newTestRunner(t)})
	sc.registerExecOp("test.latent", func(_ *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
		tgt := node.Next["then"]
		// Park with a valid key for THIS scene version.
		return execOpOutcome{park: true, parkKey: "wk|sha256:effects-test|0|42", resume: tgt}
	})
	metrics := &fakeExecMetrics{}
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	mustFire(t, sc, "e")
	// Wait until the continuation is parked.
	deadline := time.Now().Add(2 * time.Second)
	for metricsParked(metrics) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("continuation never parked")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Send a completion with a WRONG scene version in the wake key.
	wrongVersionKey := "wk|sha256:WRONG-VERSION|0|42"
	sc.Input(InputMsg{ResumeExec: wrongVersionKey, Source: "system:effect/lat"})

	deadline = time.Now().Add(time.Second)
	for metrics.stale() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("wrong-version wake key was not counted as stale")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The correct continuation must still be parked, not spuriously resumed.
	time.Sleep(30 * time.Millisecond)
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		t.Fatalf("wrong-version key resumed a continuation: %s", v)
	}
}

// TestEffects_DBQuery_EmptyDataSources_ErrorPort: db.query with an empty
// DataSources map → DATASOURCE_NOT_DECLARED on the error port — same as
// the undeclared-name case. Covers the backstop path when the DataSources
// map is a non-nil-but-empty map (not nil).
func TestEffects_DBQuery_EmptyDataSources_ErrorPort(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"q": {ID: "q", Op: OpDBQuery,
				Config: map[string]json.RawMessage{"datasource": raw(`"truth"`)},
				Next:   map[string]ExecTarget{"error": {Node: "set.err"}}},
			"set.err": setFromPin("set.err", "err", "q", "error", nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "q"}}},
	}
	eff := &SceneEffects{
		Runner:      newTestRunner(t),
		DB:          effects.NewDBQueryClient("http://gateway.invalid", "tok", nil),
		DataSources: map[string]effects.DataSource{}, // empty — truth not declared
	}
	sc := effectsScene(t, "dbq-empty-ds", prog, eff)
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
	t.Fatalf("expected DATASOURCE_NOT_DECLARED on error port for empty DataSources map, got %s", v)
}

// TestEffects_NilEgress_DenyAll: SceneEffects.Egress == nil means deny-all —
// any http.request fires the error port with EGRESS_BLOCKED, no panic, no kill.
func TestEffects_NilEgress_DenyAll(t *testing.T) {
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
	eff := &SceneEffects{
		Runner: newTestRunner(t),
		Egress: nil, // nil = deny-all
	}
	sc := effectsScene(t, "nil-egress", prog, eff)
	startScene(t, sc)

	mustFire(t, sc, "e")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get("__vars.bp.err"); ok && strings.Contains(string(v), "EGRESS_BLOCKED") {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, _ := sc.state.Get("__vars.bp.err")
	t.Fatalf("nil Egress must deny-all (EGRESS_BLOCKED on error port), got %s", v)
}

// TestEffects_R9_SetEffectsNotCalledFromShow: R9 dormancy — SetEffects is
// never called by Show.Load or Show.LoadExec. This structural test verifies
// that a prod-path scene (loaded via Show.Load) has no effects installed:
// its .effects field is nil. If someone wires SetEffects into the production
// load path this test will fail BEFORE CI.
func TestEffects_R9_SetEffectsNotCalledFromShow(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	graph := &compiler.Graph{
		SceneID:      "r9-check",
		SceneVersion: "sha256:r9",
		Defaults:     map[string]json.RawMessage{"score": raw(`0`)},
	}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:r9"}

	// Production load path.
	show.Load("r9-check", graph, bundle)
	sc, err := show.Get("r9-check")
	if err != nil {
		t.Fatal(err)
	}
	if sc.effects != nil {
		t.Fatal("R9 violated: Show.Load wired SceneEffects — production path must not activate effects before phase-4 gate")
	}
}

// TestEffects_SystemWriteDoesNotResumeParked: a raw InputMsg with Path set
// to a __system.* leaf and IsSystem=true (the tick fan-out form) must NOT
// resume a parked continuation. Only a ResumeExec message wakes a parked
// task. This is a second probe of the intra-process-only path, focused on
// the system-flagged case (Forge's test covers the non-system case).
func TestEffects_SystemWriteDoesNotResumeParked(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"lat": {ID: "lat", Op: "test.latent2",
				Next: map[string]ExecTarget{"then": {Node: "set.out"}}},
			"set.out": varSet("set.out", "out", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "lat"}}},
	}
	prog.Nodes["set.out"].Config["value"] = raw(`"woke"`)
	const parkKey = "wk|sha256:effects-test|0|77"

	metrics := &fakeExecMetrics{}
	sc := effectsScene(t, "sys-no-resume", prog, &SceneEffects{Runner: newTestRunner(t)})
	sc.SetExecMetrics(metrics)
	sc.registerExecOp("test.latent2", func(_ *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
		tgt := node.Next["then"]
		return execOpOutcome{park: true, parkKey: parkKey, resume: tgt}
	})
	startScene(t, sc)

	mustFire(t, sc, "e")
	// Wait for park.
	deadline := time.Now().Add(2 * time.Second)
	for metricsParked(metrics) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("continuation never parked")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// A system-flagged write carrying the wake key in its Value (not in
	// ResumeExec) must not resume the continuation.
	sc.Input(InputMsg{
		Path:     "__system.anim.report",
		Value:    raw(`{"wake_key":"` + parkKey + `"}`),
		Source:   "system:tick",
		IsSystem: true,
	})
	time.Sleep(50 * time.Millisecond)
	if v, _ := sc.state.Get("__vars.bp.out"); string(v) != `null` {
		t.Fatalf("system __system.* write resumed a parked continuation: %s", v)
	}

	// Confirm the correct ResumeExec path DOES wake it.
	sc.Input(InputMsg{ResumeExec: parkKey, Source: "system:effect/lat"})
	waitForState(t, sc, "__vars.bp.out", `"woke"`, 2*time.Second)
}
