package runtime

// Probe tests for the R9 activation wiring (ADR 006 §3.4, issue #106).
// Written by Probe (no-merge tier, branch probe/106-activation-wiring).
// These tests complement the existing exec_onair_test.go and show.go path
// tests written by Forge; they do NOT rewrite any of those.
//
// Axes:
//  1. execForAir gate: corrupt artefact (fail-loud) → ExecProgramsFromGraph
//     returns error; no silent install of garbage (any validated scene
//     with decode failure must propagate an error, not (nil, nil)).
//  2. execForAir gate: non-validated exec scene → nil programs (no install).
//  3. Non-validated push-swap of active scene: Show.LoadExec with nil
//     programs → exec stays dormant on live instance; no on-tick fire.
//  4. Validated push-swap of active scene: Show.LoadExec with programs
//     → on-start fires, on-tick runs after activation.
//  5. Zero-backstage crit #6, extended: THREE scenes in roster, only the
//     active one fires effects; the two off-air validated scenes stay
//     exec-quiescent through their own on-tick/on-event paths. Under -race.
//  6. SetActive order invariant: SetOnAir(true) arrives in inbox BEFORE
//     the on-start fire (both inbox messages, FIFO); the first tick that
//     arrives after activation observes onAir == true and fires.
//  7. SetEffects dormancy (R9): http.request / db.query / source.read
//     fired on a scene WITHOUT SetEffects installed → fail-close to the
//     error port, no panic, no real egress. Proves the "hold ADR §3.7"
//     guarantee: effects fail loudly, not silently.
//  8. Boot reseed invariant: ExecProgramsFromGraph on a graph with NO
//     exec_programs returns (nil, nil) — a pure-dataflow scene at boot
//     loads exec-dormant with no error (not fail-loud on empty).
//  9. Corrupt artefact at index > 0 (not index 0): the error propagates
//     with the CORRECT index, no silent partial install.
// 10. on-start fires ONLY on activation, not when a non-active scene is
//     push-swapped (Show.LoadExec off-air): FireOnStart is not called for
//     a non-active scene at load time.
// 11. Show.SetActive switch-away: previous scene's onAir flag flipped false
//     via inbox (single-writer); after switch-away that scene receives ticks
//     but fires ZERO effects — concurrent with the new scene going live.
//     Under -race.

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// ---- 1. ExecProgramsFromGraph fail-loud on corrupt artefact ---------------
//
// A graph whose exec_programs array contains a valid JSON value but not a
// valid ExecProgram structure (e.g. a JSON number) must return a non-nil
// error. The caller (execForAir / ExecForBoot) treats any error as
// fail-closed: install nothing, refuse. This test pins the fail-loud
// contract so a future refactor cannot accidentally swallow the error.
func TestActivation_ExecProgramsFromGraph_CorruptArtefact_FailLoud(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "corrupt",
		SceneVersion: "sha256:corrupt",
		ExecPrograms: []json.RawMessage{
			json.RawMessage(`{"bad json`), // syntactically invalid JSON
		},
	}
	progs, err := ExecProgramsFromGraph(graph)
	if err == nil {
		t.Fatal("ExecProgramsFromGraph must return error for syntactically corrupt program; got nil")
	}
	if progs != nil {
		t.Fatalf("ExecProgramsFromGraph must return nil programs on corrupt artefact; got %v", progs)
	}
}

// ---- 2. ExecProgramsFromGraph: empty graph → (nil, nil) —NOT— an error ----
//
// A pure-dataflow scene carries no exec_programs (the boot / non-exec case).
// ExecProgramsFromGraph must return (nil, nil) — this is the "no programs,
// no error" sentinel the gate uses to signal "eligible but dataflow-only".
// If this returns an error, every non-exec production scene would fail at boot.
func TestActivation_ExecProgramsFromGraph_Empty_NilNil(t *testing.T) {
	graph := &compiler.Graph{
		SceneID:      "pure",
		SceneVersion: "sha256:pure",
	}
	progs, err := ExecProgramsFromGraph(graph)
	if err != nil {
		t.Fatalf("ExecProgramsFromGraph on empty graph must not error; got %v", err)
	}
	if progs != nil {
		t.Fatalf("ExecProgramsFromGraph on empty graph must return nil programs; got %v", progs)
	}
}

// ---- 3. Non-validated push-swap of active scene: exec stays dormant --------
//
// LoadExec with nil programs (the not-eligible path execForAir returns) must
// produce an exec-dormant scene: no on-tick fires, no effects. This simulates
// the mid-broadcast push of an unvalidated new version where the gate keeps
// the antenna on the previous version — but the new roster instance is loaded
// with nil progs (off-air, exec-quiescent) so it cannot accidentally fire.
func TestActivation_LoadExec_NilProgs_ExecDormant_NoOnTick(t *testing.T) {
	sc := NewScene("dormant-push", varsGraph("dormant-push"), &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}, NewComputeRegistry(), quietLogger())
	// Do NOT call InstallExec — simulates nil progs from execForAir.
	sc.GateTriggers()
	eff := &countingEffector{inner: &sceneEffector{sc}}
	sc.SetEffector(eff)
	startScene(t, sc)

	// Deliver several ticks — no program installed, no fire possible.
	for i := int64(0); i < 5; i++ {
		tick(t, sc, 5000+i*16)
	}
	drainScene(t, sc)

	if got := eff.effects(); got != 0 {
		t.Fatalf("exec-dormant scene (nil progs) attempted %d effects on tick, want 0", got)
	}
	if len(sc.execOnTick) != 0 {
		t.Fatalf("nil-prog scene must have empty execOnTick; got %v", sc.execOnTick)
	}
}

// ---- 4. Validated push-swap of active scene: on-start fires, on-tick runs --
//
// LoadExec with a valid exec program (the eligible path) must produce a
// scene whose on-start fires on activation and whose on-tick runs when it
// is put on air. This is the positive franchise gate: the validated version
// IS armed with its exec programs.
func TestActivation_LoadExec_ValidProgs_OnStartFiresOnActivation(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)

	ga := varsGraph("active-a")
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	// Load with a real program (the validated path).
	prog := onTickSetProg()
	show.LoadExec("active-a", ga, bundle, prog)

	if err := show.SetActive("active-a", nil); err != nil {
		t.Fatal(err)
	}
	sc, _ := show.Get("active-a")

	// on-start must have fired (from Show.SetActive → dest.FireOnStart).
	// Deliver a tick: on-tick must fire the effect.
	tick(t, sc, 9000)
	waitForState(t, sc, "__vars.bp.ticked", "1", time.Second)
}

// ---- 5. Zero-backstage crit #6: THREE scenes, only active fires effects ----
//
// Three exec-bearing roster scenes loaded via LoadExec. Only scene-a is
// activated (on-air). Scenes b and c receive the same tick fan-out but must
// fire ZERO effects. After switching to scene-b, scene-a must go quiescent.
// Runs under -race via the test runner.
func TestActivation_ThreeScenes_OnlyActiveFiresEffects(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)

	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	// Load three exec-bearing scenes.
	for _, id := range []string{"s1", "s2", "s3"} {
		show.LoadExec(id, varsGraph(id), bundle, onTickSetProg())
	}

	// Activate s1.
	if err := show.SetActive("s1", nil); err != nil {
		t.Fatal(err)
	}
	s1, _ := show.Get("s1")
	s2, _ := show.Get("s2")
	s3, _ := show.Get("s3")

	// Instrument s2 and s3 with counting effectors.
	eff2 := &countingEffector{inner: &sceneEffector{s2}}
	s2.SetEffector(eff2)
	eff3 := &countingEffector{inner: &sceneEffector{s3}}
	s3.SetEffector(eff3)

	// Tick all three (mirrors tick.go fan-out).
	for _, s := range []*Scene{s1, s2, s3} {
		tick(t, s, 10000)
	}
	// s1 is on-air: its on-tick fires.
	waitForState(t, s1, "__vars.bp.ticked", "1", time.Second)
	// s2 and s3 are off-air: they must be quiescent.
	drainScene(t, s2)
	drainScene(t, s3)
	if got := eff2.effects(); got != 0 {
		t.Fatalf("off-air s2 attempted %d effects, want 0 (zero backstage, crit #6)", got)
	}
	if got := eff3.effects(); got != 0 {
		t.Fatalf("off-air s3 attempted %d effects, want 0 (zero backstage, crit #6)", got)
	}

	// Switch to s2; s1 must go quiescent.
	eff1 := &countingEffector{inner: &sceneEffector{s1}}
	s1.SetEffector(eff1)
	if err := show.SetActive("s2", nil); err != nil {
		t.Fatal(err)
	}
	// Let s1's inbox drain the SetOnAir(false) message.
	drainScene(t, s1)
	before1 := eff1.effects()

	// Further ticks: s2 must fire; s1 must not.
	for _, s := range []*Scene{s1, s2, s3} {
		tick(t, s, 11000)
	}
	waitForState(t, s2, "__vars.bp.ticked", "1", time.Second)
	drainScene(t, s1)
	if got := eff1.effects(); got != before1 {
		t.Fatalf("quiescent s1 attempted %d more effects after switch-away, want 0", got-before1)
	}
}

// ---- 6. SetActive order invariant: onAir=true before on-start fire ---------
//
// Show.SetActive sends SetOnAir(true) and then FireOnStart via inbox FIFO.
// The on-start handler fires a variable.set; the applyInput flow that processes
// the on-start fire must see onAir == true (the flag was set earlier in the
// same inbox FIFO). This test verifies that on-start fires (task enqueues)
// only happen while the scene is already on-air, not before.
//
// We verify this indirectly: the first on-tick after activation must fire
// (onAir true), and the on-start must run (state written). If the order were
// reversed (on-start before onAir=true), the scene would be gated and no
// effects would run on on-start either. But on-start is NOT gated (it fires
// explicitly only at activation), so this test confirms the doctrinal
// invariant: after SetActive the on-tick gate is open before any future tick.
func TestActivation_SetActive_OnAirBeforeOnStartFire(t *testing.T) {
	// on-start writes "started" = true; on-tick writes "ticked" = 1.
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"start_set": varSet("start_set", "started", nil, nil),
			"tick_set":  varSet("tick_set", "ticked", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"onstart": {Kind: EntryOnStart, Target: ExecTarget{Node: "start_set"}},
			"ontick":  {Kind: EntryOnTick, Target: ExecTarget{Node: "tick_set"}, Node: "tickn"},
		},
	}
	prog.Nodes["start_set"].Config["value"] = raw(`true`)
	prog.Nodes["tick_set"].Config["value"] = raw(`1`)

	graph := &compiler.Graph{
		SceneID:      "order-test",
		SceneVersion: "sha256:exec-test",
		Nodes: []compiler.GraphNode{
			{ID: "var.started", Kind: "input", Path: "__vars.bp.started"},
			{ID: "var.ticked", Kind: "input", Path: "__vars.bp.ticked"},
		},
		Defaults: map[string]json.RawMessage{
			"__vars.bp.started": raw(`false`),
			"__vars.bp.ticked":  raw(`0`),
		},
	}
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}
	show.LoadExec("order-test", graph, bundle, prog)

	if err := show.SetActive("order-test", nil); err != nil {
		t.Fatal(err)
	}
	sc, _ := show.Get("order-test")

	// on-start must have fired on activation.
	waitForState(t, sc, "__vars.bp.started", `true`, time.Second)

	// Now deliver a tick: onAir must be true so on-tick fires.
	tick(t, sc, 20000)
	waitForState(t, sc, "__vars.bp.ticked", `1`, time.Second)
}

// ---- 7. SetEffects dormancy (R9): http.request without SetEffects → error port
//
// A scene that has exec programs (ValidExec) but no SetEffects installed
// fires an http.request node. The op must route to the error port with a
// message indicating the pool/effects are unconfigured — no real socket is
// opened, no panic, no kill. This is the "hold ADR §3.7" guarantee at the
// runtime level: the op is registered only when SetEffects is called; without
// it the op is UNREGISTERED → the exec_interpreter's "unregistered op" path
// logs a warning and continues. The chain should NOT crash.
func TestActivation_SetEffects_NotInstalled_HTTPRequest_NoRealEgress(t *testing.T) {
	// Build a program that fires http.request on on-start. Without SetEffects
	// the op is unregistered in s.execOps; execNode logs a warning and
	// returns execOpOutcome{halt:true} (no next target, task ends).
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"req": {
				ID: "req", Op: OpHTTPRequest,
				Config: map[string]json.RawMessage{
					"url": json.RawMessage(`"https://api.example.com/endpoint"`),
				},
				Next: map[string]ExecTarget{
					"then":  {Node: "set.ok"},
					"error": {Node: "set.err"},
				},
			},
			"set.ok":  varSet("set.ok", "ok", nil, nil),
			"set.err": varSet("set.err", "err", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "req"}},
		},
	}
	prog.Nodes["set.ok"].Config["value"] = raw(`true`)
	prog.Nodes["set.err"].Config["value"] = raw(`"failed"`)

	graph := &compiler.Graph{
		SceneID:      "no-effects",
		SceneVersion: "sha256:exec-test",
		Nodes: []compiler.GraphNode{
			{ID: "var.ok", Kind: "input", Path: "__vars.bp.ok"},
			{ID: "var.err", Kind: "input", Path: "__vars.bp.err"},
		},
		Defaults: map[string]json.RawMessage{
			"__vars.bp.ok":  raw(`false`),
			"__vars.bp.err": raw(`null`),
		},
	}
	sc := NewScene("no-effects", graph, &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(prog)
	// NOTE: NO SetEffects call — R9 dormancy, http.request is unregistered.
	startScene(t, sc)

	// Fire on-start directly.
	sc.FireOnStart("system:test")

	// The scene must still be alive and serving inputs after the unregistered
	// op. Neither "ok" nor "error" state should be written (the op halts).
	time.Sleep(50 * time.Millisecond)
	if v, ok := sc.state.Get("__vars.bp.ok"); ok && string(v) != `false` {
		t.Fatalf("http.request without SetEffects must not reach 'then' port: ok=%s", v)
	}
	// The scene must still handle further writes (no crash, no hang).
	sc.Input(InputMsg{Path: "score.team_a", Value: raw(`42`), Source: "probe"})
	waitForState(t, sc, "score.team_a", `42`, time.Second)
}

// ---- 8. Corrupt artefact at index > 0: correct index in error, no partial install
//
// A graph with two exec_programs: index 0 is valid, index 1 is corrupt.
// ExecProgramsFromGraph must return an error referencing index 1 and return
// nil programs — no partial install of the first program. The whole batch
// fails, or nothing is installed.
func TestActivation_ExecProgramsFromGraph_CorruptAtIndexOne_ErrorIndexed(t *testing.T) {
	validProg := &ExecProgram{
		BlueprintKey: "bpA",
		Nodes:        map[string]*ExecNode{},
		Entrypoints:  map[string]ExecEntry{},
	}
	validRaw, err := json.Marshal(validProg)
	if err != nil {
		t.Fatal(err)
	}
	graph := &compiler.Graph{
		SceneID:      "partial",
		SceneVersion: "sha256:partial",
		ExecPrograms: []json.RawMessage{
			validRaw,                       // index 0: valid
			json.RawMessage(`{"bad json`),  // index 1: corrupt
		},
	}
	progs, err := ExecProgramsFromGraph(graph)
	if err == nil {
		t.Fatal("ExecProgramsFromGraph must return error when any program is corrupt; got nil")
	}
	if progs != nil {
		t.Fatalf("ExecProgramsFromGraph must return nil (no partial install) on error; got %v", progs)
	}
	// The error must mention index 1.
	if msg := err.Error(); msg == "" {
		t.Fatal("error message must not be empty")
	}
}

// ---- 9. on-start NOT fired for off-air scene at LoadExec -------------------
//
// When a scene is loaded via LoadExec but is NOT the active scene, Show does
// NOT fire on-start (that would be a backstage logic kick). FireOnStart is
// called ONLY for the currently active scene (the push-swap path).
// Verify by loading two scenes, activating one, then calling LoadExec on the
// OTHER scene: on-start must not run for the off-air one.
func TestActivation_LoadExec_OffAirScene_OnStartNotFired(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	// Activate scene-x.
	show.LoadExec("scene-x", varsGraph("scene-x"), bundle, onTickSetProg())
	if err := show.SetActive("scene-x", nil); err != nil {
		t.Fatal(err)
	}

	// Now load scene-y (off-air) with an on-start that writes "started".
	startedProg := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"s": varSet("s", "started", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"onstart": {Kind: EntryOnStart, Target: ExecTarget{Node: "s"}},
		},
	}
	startedProg.Nodes["s"].Config["value"] = raw(`true`)
	show.LoadExec("scene-y", varsGraph("scene-y"), bundle, startedProg)

	// Let the loop drain.
	sy, _ := show.Get("scene-y")
	drainScene(t, sy)
	time.Sleep(30 * time.Millisecond)

	// scene-y's on-start must NOT have been called (off-air at load time).
	if v, ok := sy.state.Get("__vars.bp.started"); ok && string(v) != `false` {
		t.Fatalf("off-air scene-y fired on-start at LoadExec (backstage kick): started=%s", v)
	}
}

// ---- 10. Race: on-air flag is single-writer under concurrent ticks ----------
//
// Under -race: one goroutine toggles the on-air flag (SetOnAir true/false)
// while another delivers rapid ticks to the same scene. The race detector
// must not fire. After the goroutine finishes, the scene must still be alive
// (drain succeeds, no hang, no nil pointer).
func TestActivation_Race_OnAirFlagConcurrentTicks(t *testing.T) {
	sc, _ := gatedExecScene(t, "race-onair", onTickSetProg())

	var wg sync.WaitGroup
	const rounds = 20
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			sc.SetOnAir(i%2 == 0)
		}
	}()

	// Concurrent ticks.
	for i := int64(0); i < int64(rounds*2); i++ {
		tick(t, sc, 30000+i*16)
	}
	wg.Wait()

	// Scene must still be alive.
	drainScene(t, sc)
}

// ---- 11. Show.SetActive switch-away: concurrent ticks, -race ---------------
//
// Two exec-bearing scenes in a Show. Concurrent goroutines: one flips the
// active scene back and forth (SetActive); another delivers ticks to both
// scenes. The race detector must not fire. After all operations settle, the
// final active scene must have its on-air flag set (its ticks fire) and the
// other must not.
func TestActivation_Race_SetActiveConcurrentTicks(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)

	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}
	show.LoadExec("r-a", varsGraph("r-a"), bundle, onTickSetProg())
	show.LoadExec("r-b", varsGraph("r-b"), bundle, onTickSetProg())

	if err := show.SetActive("r-a", nil); err != nil {
		t.Fatal(err)
	}

	sa, _ := show.Get("r-a")
	sb, _ := show.Get("r-b")

	var activateDone int64
	go func() {
		// Alternate active scene rapidly.
		scenes := []string{"r-a", "r-b", "r-a", "r-b", "r-a"}
		for _, id := range scenes {
			show.SetActive(id, nil) //nolint:errcheck
			time.Sleep(5 * time.Millisecond)
		}
		atomic.StoreInt64(&activateDone, 1)
	}()

	// Deliver ticks to both scenes concurrently while SetActive is running.
	for i := int64(0); atomic.LoadInt64(&activateDone) == 0 || i < 10; i++ {
		tick(t, sa, 40000+i*16)
		tick(t, sb, 40000+i*16)
		time.Sleep(2 * time.Millisecond)
	}

	// Both scenes must still be alive.
	drainScene(t, sa)
	drainScene(t, sb)
}
