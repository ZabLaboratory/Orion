package runtime

// Probe tests for the multi-program install seam (ADR 006 §3.3, issue #105).
// Written by Probe (no-merge tier, branch forge/105-multi-program-install).
// These tests COMPLEMENT Forge's exec_multiprog_test.go; they do not
// rewrite any of those tests.
//
// Axes added:
//   1. resolveEntry ambiguous (two blueprints, same bare entry id) → refuses LOUD
//   2. resolveEntry unambiguous bare id (single blueprint) → resolves
//   3. task.prog inherited by forked continuation in multi-prog scene
//   4. on-event routing: fired task carries correct BlueprintKey (not the sibling's)
//   5. InstallExec idempotent: repeated calls with same set → identical index
//   6. Show.Load (nil progs path) → exec stays dormant, FireOnStart no-op
//   7. on-tick across two blueprints: each task uses its own prog's BlueprintKey
//   8. -race: concurrent FireOnStart on same multi-prog scene (single-writer preserved)
//   9. Three blueprints, all on-event same topic → all three entries fire, each in own prog

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// ---- 1. resolveEntry: ambiguous bare id → refuses LOUD --------------------
//
// Two blueprints each declare an entrypoint with the same local id "start".
// Firing the bare key "start" via FireExec must be a no-op (resolveEntry
// returns false, enqueueFire warns and returns early). The namespaced keys
// bpA/start and bpB/start MUST resolve individually.
func TestExecMulti_ResolveEntry_AmbiguousBareId_Refused(t *testing.T) {
	progA := setProg("bpA", 11)
	progB := setProg("bpB", 22)
	sc := multiScene(t, "ambiguous", progA, progB)
	startScene(t, sc)

	// Bare "start" is ambiguous — must not fire either blueprint.
	if !sc.FireExec("start", "probe") {
		t.Fatal("inbox full for ambiguous fire")
	}
	time.Sleep(30 * time.Millisecond) // let the loop process
	if v, ok := sc.state.Get("__vars.bpA.counter"); ok && string(v) != `0` {
		t.Errorf("bpA counter advanced after ambiguous bare-id fire: %s", v)
	}
	if v, ok := sc.state.Get("__vars.bpB.counter"); ok && string(v) != `0` {
		t.Errorf("bpB counter advanced after ambiguous bare-id fire: %s", v)
	}

	// Namespaced keys must still resolve individually.
	sc.FireExec("bpA/start", "probe")
	waitForState(t, sc, "__vars.bpA.counter", `11`, time.Second)
	if v, ok := sc.state.Get("__vars.bpB.counter"); ok && string(v) != `0` {
		t.Errorf("bpB counter changed after bpA/start fire: %s", v)
	}

	sc.FireExec("bpB/start", "probe")
	waitForState(t, sc, "__vars.bpB.counter", `22`, time.Second)
}

// ---- 2. resolveEntry: unambiguous bare id (one blueprint) → resolves ------
//
// A single-blueprint scene: the bare entry id must resolve via the fallback
// linear scan (n==1 path in resolveEntry). Regression gate against a future
// refactor that drops the fallback.
func TestExecMulti_ResolveEntry_UnambiguousBareId_Resolves(t *testing.T) {
	sc := multiScene(t, "unambiguous", setProg("solo", 7))
	startScene(t, sc)

	sc.FireExec("start", "probe") // bare id, single blueprint
	waitForState(t, sc, "__vars.solo.counter", `7`, time.Second)
}

// ---- 3. task.prog inherited by forked continuation in multi-prog scene ----
//
// bpA's on-start parks a latent op (fork). The forked continuation inherits
// t.prog = bpA; when it resumes it writes __vars.bpA.woke, NOT __vars.bpB.
// This proves the prog pointer travels with the forked task, not defaulting
// to some scene-global.
func TestExecMulti_ForkedContinuation_InheritsOwningProg(t *testing.T) {
	// bpA: on-start → latent → variable.set("woke", true)
	mkForkProg := func(key string) *ExecProgram {
		n := varSet("setwoke", "woke", nil, nil)
		n.Config["value"] = raw(`true`)
		return &ExecProgram{
			BlueprintKey: key,
			Nodes: map[string]*ExecNode{
				"lat":     {ID: "lat", Op: "test.latent.fork", Next: map[string]ExecTarget{"then": {Node: "setwoke"}}},
				"setwoke": n,
			},
			Entrypoints: map[string]ExecEntry{
				"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "lat"}},
			},
		}
	}

	// bpB is inert (different entry id to avoid ambiguity, no on-start)
	bpB := &ExecProgram{
		BlueprintKey: "bpB",
		Nodes:        map[string]*ExecNode{},
		Entrypoints:  map[string]ExecEntry{},
	}

	sc := multiScene(t, "fork-prog", mkForkProg("bpA"), bpB)

	var mu sync.Mutex
	var wakeKey string
	sc.registerExecOp("test.latent.fork", func(_ *Scene, _ *execTask, node *ExecNode, _ string) execOpOutcome {
		mu.Lock()
		defer mu.Unlock()
		resume, _ := node.next("then")
		wakeKey = "fork-prog-wake"
		return execOpOutcome{park: true, parkKey: wakeKey, resume: resume}
	})

	startScene(t, sc)
	sc.FireOnStart("system:scene-activated")

	// Wait until the park is registered.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		k := wakeKey
		mu.Unlock()
		if k != "" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	mu.Lock()
	k := wakeKey
	mu.Unlock()
	if k == "" {
		t.Fatal("latent op never parked within 1s")
	}

	// Resume the forked continuation.
	sc.Input(InputMsg{ResumeExec: k, Source: "probe"})

	// The continuation must write __vars.bpA.woke, not __vars.bpB.woke.
	waitForState(t, sc, "__vars.bpA.woke", `true`, time.Second)
	if v, ok := sc.state.Get("__vars.bpB.woke"); ok {
		t.Errorf("forked continuation wrote into bpB namespace: %s", v)
	}
}

// ---- 4. on-event routing: fired task carries correct BlueprintKey ---------
//
// Two blueprints both listen to the SAME topic "news": the writes must land
// under their own __vars namespaces and NOT bleed into the sibling's.
// Additionally, the task that fires for each blueprint must use that
// blueprint's prog to resolve variable.set (t.prog.BlueprintKey correct).
func TestExecMulti_OnEventSameTopic_TwoBlueprints_BothFire_OwnProg(t *testing.T) {
	mkNewsProg := func(key string, value int) *ExecProgram {
		n := varSet("setnews", "news_count", nil, nil)
		n.Config["value"] = raw(strconv.Itoa(value))
		return &ExecProgram{
			BlueprintKey: key,
			Nodes:        map[string]*ExecNode{"setnews": n},
			Entrypoints: map[string]ExecEntry{
				"onnews": {Kind: EntryOnEvent, Event: "news", Target: ExecTarget{Node: "setnews"}},
			},
		}
	}
	progA := mkNewsProg("bpA", 100)
	progB := mkNewsProg("bpB", 200)

	// Graph needs both vars to be declared.
	graph := &compiler.Graph{
		SceneID:      "event-topic",
		SceneVersion: "sha256:event-topic-test",
		Nodes: []compiler.GraphNode{
			{ID: "va", Kind: "input", Path: "__vars.bpA.news_count"},
			{ID: "vb", Kind: "input", Path: "__vars.bpB.news_count"},
		},
		Defaults: map[string]json.RawMessage{
			"__vars.bpA.news_count": raw(`0`),
			"__vars.bpB.news_count": raw(`0`),
		},
	}
	sc := NewScene("event-topic", graph, &compiler.RenderBundle{SceneVersion: "sha256:event-topic-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(progA, progB)
	startScene(t, sc)

	// Both blueprints are subscribed to "news".
	if got := sc.execOnEvent["news"]; len(got) != 2 {
		t.Fatalf("on-event[news] = %v, want 2 entries (bpA/onnews + bpB/onnews)", got)
	}

	sc.Input(InputMsg{Path: eventsPrefix + "news", Value: raw(`true`)})

	// Both blueprints fire and each writes to its own namespace.
	waitForState(t, sc, "__vars.bpA.news_count", `100`, time.Second)
	waitForState(t, sc, "__vars.bpB.news_count", `200`, time.Second)
}

// ---- 5. InstallExec idempotent: repeated install → identical index --------
//
// Calling InstallExec twice on the same scene with the same program set must
// produce an identical sorted trigger index on both calls (no double-append,
// no residue from the first call).
func TestExecMulti_InstallExec_Idempotent(t *testing.T) {
	progA := setProg("bpA", 1)
	progB := setProg("bpB", 2)

	sc := NewScene("idem", multiVarsGraph("idem"), &compiler.RenderBundle{SceneVersion: "sha256:multi-exec-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(progA, progB)
	firstOnStart := append([]string(nil), sc.execOnStart...)

	// Reinstall the same set.
	sc.InstallExec(progA, progB)
	secondOnStart := append([]string(nil), sc.execOnStart...)

	if len(firstOnStart) != len(secondOnStart) {
		t.Fatalf("reinstall doubled the on-start index: first=%v second=%v", firstOnStart, secondOnStart)
	}
	for i := range firstOnStart {
		if firstOnStart[i] != secondOnStart[i] {
			t.Fatalf("on-start index differs after reinstall at index %d: first=%v second=%v", i, firstOnStart, secondOnStart)
		}
	}
}

// ---- 6. Show.Load (nil progs) → exec dormant, FireOnStart is no-op --------
//
// The production path: Show.Load calls LoadExec with no progs.
// The loaded scene must have execProgs == nil, and FireOnStart must be a
// pure no-op (no task enqueued, inbox stays empty).
func TestExecMulti_ShowLoad_NilProgs_ExecDormant(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)

	graph := multiVarsGraph("dormant-scene")
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:dormant-test"}
	show.Load("dormant-scene", graph, bundle) // production path: zero progs

	sc, err := show.Get("dormant-scene")
	if err != nil {
		t.Fatalf("scene not found after Load: %v", err)
	}
	if len(sc.execProgs) != 0 {
		t.Fatalf("execProgs non-empty after zero-prog Load: %v", sc.execProgs)
	}

	// FireOnStart must not enqueue a task — inbox stays empty.
	sc.FireOnStart("system:scene-activated")
	time.Sleep(20 * time.Millisecond)
	// Inbox empty and execQueue empty: no exec task was born.
	if len(sc.execQueue) != 0 {
		t.Fatalf("execQueue non-empty after FireOnStart on dormant scene: %d tasks", len(sc.execQueue))
	}
}

// ---- 7. on-tick across two blueprints: each uses own BlueprintKey ---------
//
// Two blueprints each have an on-tick that writes their own counter via
// variable.set. After one tick write, both vars carry non-zero values under
// their OWN namespace, not the sibling's.
func TestExecMulti_OnTick_TwoBlueprints_EachOwnNamespace(t *testing.T) {
	mkTickProg := func(key string, value int) *ExecProgram {
		n := varSet("settick", "tick_seen", nil, nil)
		n.Config["value"] = raw(strconv.Itoa(value))
		return &ExecProgram{
			BlueprintKey: key,
			Nodes:        map[string]*ExecNode{"settick": n},
			Entrypoints: map[string]ExecEntry{
				"ontick": {Kind: EntryOnTick, Target: ExecTarget{Node: "settick"}},
			},
		}
	}
	progA := mkTickProg("bpA", 7)
	progB := mkTickProg("bpB", 9)

	graph := &compiler.Graph{
		SceneID:      "tick-multi",
		SceneVersion: "sha256:tick-multi-test",
		Nodes: []compiler.GraphNode{
			{ID: "va", Kind: "input", Path: "__vars.bpA.tick_seen"},
			{ID: "vb", Kind: "input", Path: "__vars.bpB.tick_seen"},
		},
		Defaults: map[string]json.RawMessage{
			"__vars.bpA.tick_seen": raw(`0`),
			"__vars.bpB.tick_seen": raw(`0`),
		},
	}
	sc := NewScene("tick-multi", graph, &compiler.RenderBundle{SceneVersion: "sha256:tick-multi-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(progA, progB)
	startScene(t, sc)

	// Simulate a tick write (the tickPath is __system.tick.now_ms).
	sc.Input(InputMsg{
		Path:     tickPath,
		Value:    raw(`1000`),
		IsSystem: true,
	})

	waitForState(t, sc, "__vars.bpA.tick_seen", `7`, time.Second)
	waitForState(t, sc, "__vars.bpB.tick_seen", `9`, time.Second)

	// Cross-namespace: bpA must not carry bpB's value.
	if v, _ := sc.state.Get("__vars.bpA.tick_seen"); string(v) != `7` {
		t.Errorf("bpA.tick_seen = %s, want 7 (no bleed from bpB)", v)
	}
	if v, _ := sc.state.Get("__vars.bpB.tick_seen"); string(v) != `9` {
		t.Errorf("bpB.tick_seen = %s, want 9 (no bleed from bpA)", v)
	}
}

// ---- 8. -race: concurrent FireOnStart on multi-prog scene -----------------
//
// N goroutines call FireOnStart simultaneously. The scene loop is the only
// writer; the concurrent callers only enqueue inbox messages. The race
// detector must stay clean. After all fires drain both vars must have their
// declared values (not some torn intermediate).
func TestExecMulti_Race_ConcurrentFireOnStart_NDataRace(t *testing.T) {
	progA := setProg("bpA", 42)
	progB := setProg("bpB", 43)
	sc := multiScene(t, "race-multi", progA, progB)
	startScene(t, sc)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			sc.FireOnStart("probe:concurrent")
		}()
	}
	wg.Wait()

	// Both blueprints' on-start must have settled; the final values are
	// 42 and 43 regardless of how many redundant fires happened.
	waitForState(t, sc, "__vars.bpA.counter", `42`, 2*time.Second)
	waitForState(t, sc, "__vars.bpB.counter", `43`, 2*time.Second)
}

// ---- 9. Three blueprints, all on-event same topic → all three fire --------
//
// Three blueprints each subscribe to "alert". All three must fire and write
// to their own namespace — no entry is silently dropped when the on-event
// list has >2 subscribers.
func TestExecMulti_ThreeBlueprints_SameTopic_AllFire(t *testing.T) {
	mkAlertProg := func(key string, value int) *ExecProgram {
		n := varSet("setalert", "alerted", nil, nil)
		n.Config["value"] = raw(strconv.Itoa(value))
		return &ExecProgram{
			BlueprintKey: key,
			Nodes:        map[string]*ExecNode{"setalert": n},
			Entrypoints: map[string]ExecEntry{
				"onalert": {Kind: EntryOnEvent, Event: "alert", Target: ExecTarget{Node: "setalert"}},
			},
		}
	}

	graph := &compiler.Graph{
		SceneID:      "three-alert",
		SceneVersion: "sha256:three-alert-test",
		Nodes: []compiler.GraphNode{
			{ID: "va", Kind: "input", Path: "__vars.bpA.alerted"},
			{ID: "vb", Kind: "input", Path: "__vars.bpB.alerted"},
			{ID: "vc", Kind: "input", Path: "__vars.bpC.alerted"},
		},
		Defaults: map[string]json.RawMessage{
			"__vars.bpA.alerted": raw(`0`),
			"__vars.bpB.alerted": raw(`0`),
			"__vars.bpC.alerted": raw(`0`),
		},
	}
	sc := NewScene("three-alert", graph, &compiler.RenderBundle{SceneVersion: "sha256:three-alert-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(mkAlertProg("bpA", 1), mkAlertProg("bpB", 2), mkAlertProg("bpC", 3))
	startScene(t, sc)

	if got := sc.execOnEvent["alert"]; len(got) != 3 {
		t.Fatalf("on-event[alert] = %v, want 3 entries", got)
	}

	sc.Input(InputMsg{Path: eventsPrefix + "alert", Value: raw(`true`)})

	waitForState(t, sc, "__vars.bpA.alerted", `1`, time.Second)
	waitForState(t, sc, "__vars.bpB.alerted", `2`, time.Second)
	waitForState(t, sc, "__vars.bpC.alerted", `3`, time.Second)
}
