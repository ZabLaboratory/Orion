package runtime

import (
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// Tests for ADR 008 Amendment 1 §A1.6 (criteria #8–12): re-activation of
// the already-active scene (`from == id`) refires `on-start`. The doctrine
// (§3.2/§3.4/R2) always said "on-start refires at every activation"; the
// code only honoured it on a transition (`from != id`). These tests pin the
// corrected contract: refire exactly once per call, state preserved (refire ≠
// reseed), no phantom `scene_changed`, switch path untouched, boot refires.

// onStartIncrProg fires `on-start` → variable.set(counter = add.counter),
// where add.counter = var.counter + lit.one (wired in varsGraph). Each fire
// therefore reads the PRIOR counter and writes prior+1. This single program
// proves two distinct things:
//   - fire COUNT: counter climbs by exactly 1 per activation call (#8).
//   - state PRESERVED: the new value is computed off the prior persisted
//     value, never reseeded to the default 0 (#9). A reseed-on-reactivate
//     bug would clamp the counter at 1 forever.
func onStartIncrProg() *ExecProgram {
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "counter",
				[]ExecDataInput{{Port: "value", From: "add.counter"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"start": {Target: ExecTarget{Node: "set"}, Kind: EntryOnStart, Node: "startn"},
		},
	}
}

// loadIncrScene loads an exec scene carrying onStartIncrProg into a Show via
// the real LoadExec seam (the same path cold start uses), so SetActive drives
// the production lifecycle. counter leaf reads __vars.bp.counter.
const incrCounterLeaf = "__vars.bp.counter"

func loadIncrScene(t *testing.T, show *Show, id string) {
	t.Helper()
	g := varsGraph(id)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}
	show.LoadExec(id, g, bundle, onStartIncrProg())
}

// TestReactivation_RefiresOnStartExactlyOnce (criterion #8): scene A active;
// a second POST /show/active-scene{A} (from == id) refires on-start exactly
// once — the counter advances by exactly 1 per re-activation, no double-fire.
func TestReactivation_RefiresOnStartExactlyOnce(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	loadIncrScene(t, show, "scene-a")
	sa, _ := show.Get("scene-a")

	// First activation (from "" -> A, a transition): on-start fires once.
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	waitForState(t, sa, incrCounterLeaf, "1", time.Second)

	// Re-activation (from == id): the fix under test. on-start must refire
	// exactly once -> counter == 2 (not 1: no missed fire; not 3: no double).
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	waitForState(t, sa, incrCounterLeaf, "2", time.Second)

	// A third re-activation keeps the cadence at exactly +1.
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	waitForState(t, sa, incrCounterLeaf, "3", time.Second)
}

// TestReactivation_PreservesStateNoReseed (criterion #9): re-activation
// refires on-start WITHOUT reseeding the scene's state. The counter, having
// reached N, is recomputed by the on-start as N+1 (read prior, write prior+1)
// — never clamped back to the default 0. Refire (yes) is distinct from reseed
// (no): the freeze-resume invariant of §3.2 is intact.
func TestReactivation_PreservesStateNoReseed(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	loadIncrScene(t, show, "scene-a")
	sa, _ := show.Get("scene-a")

	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	waitForState(t, sa, incrCounterLeaf, "1", time.Second)

	// Accumulate more live state directly (models on-tick/event writes that
	// pushed the counter past its on-start seed). The persisted live value is
	// now 7 — the thing a reseed would destroy.
	sa.Input(InputMsg{Path: incrCounterLeaf, Value: raw(`7`), Source: "test"})
	waitForState(t, sa, incrCounterLeaf, "7", time.Second)

	// Re-activate. If re-activation reseeded, the counter would snap to the
	// default 0 then on-start would write 1. The correct behaviour reads the
	// preserved 7 and writes 8.
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	waitForState(t, sa, incrCounterLeaf, "8", time.Second)
}

// TestReactivation_NoPhantomSceneChanged (criterion #10): re-activation emits
// NO scene_changed to live subscribers (from == to is not a viewer
// transition) — a fresh snapshot is emitted instead. The scene never turns
// itself off air (no SetOnAir(false) on A: it stays live throughout, proven
// by on-start refiring under the on-air gate).
func TestReactivation_NoPhantomSceneChanged(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	loadIncrScene(t, show, "scene-a")
	sa, _ := show.Get("scene-a")

	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	sub, snap, err := show.SubscribeLive(8)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SceneID != "scene-a" {
		t.Fatalf("initial snap scene_id %q", snap.SceneID)
	}
	waitForState(t, sa, incrCounterLeaf, "1", time.Second)

	// Re-activate the already-active scene.
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}

	// The subscriber must receive a fresh snapshot of A and NOT a
	// scene_changed. Collect everything that arrives within a window; assert
	// a snapshot showed up and no scene_changed ever did.
	deadline := time.After(300 * time.Millisecond)
	gotSnap := false
	for {
		select {
		case msg := <-sub.Out:
			switch m := msg.(type) {
			case *protocol.SceneChanged:
				t.Fatalf("phantom scene_changed emitted on re-activation: %s->%s",
					m.FromSceneID, m.ToSceneID)
			case *protocol.Snapshot:
				if m.SceneID != "scene-a" {
					t.Fatalf("snap scene_id %q, want scene-a", m.SceneID)
				}
				gotSnap = true
			}
		case <-deadline:
			if !gotSnap {
				t.Fatal("re-activation emitted no fresh snapshot to the live sub")
			}
			// On-start still refired despite no scene_changed: counter == 2.
			waitForState(t, sa, incrCounterLeaf, "2", time.Second)
			return
		}
	}
}

// TestReactivation_SwitchPathUntouched (criterion #11): the from != id switch
// path is unchanged — a real A->B switch still emits scene_changed{a->b} and
// the destination's on-start fires exactly once. This is the local guard that
// the gating change did not regress switch semantics; the canonical assertion
// remains TestShow_SwitchMigratesLiveSubsAndEmitsSceneChanged (run unmodified).
func TestReactivation_SwitchPathUntouched(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	loadIncrScene(t, show, "scene-a")
	loadIncrScene(t, show, "scene-b")
	sb, _ := show.Get("scene-b")

	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	sub, _, err := show.SubscribeLive(8)
	if err != nil {
		t.Fatal(err)
	}

	// Switch A -> B (from != id). scene_changed{a->b} + fresh snapshot of B.
	if err := show.SetActive("scene-b", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	gotChanged, gotSnap := false, false
	for !(gotChanged && gotSnap) {
		select {
		case msg := <-sub.Out:
			switch m := msg.(type) {
			case *protocol.SceneChanged:
				if m.FromSceneID != "scene-a" || m.ToSceneID != "scene-b" {
					t.Fatalf("changed %s->%s, want scene-a->scene-b", m.FromSceneID, m.ToSceneID)
				}
				gotChanged = true
			case *protocol.Snapshot:
				if m.SceneID == "scene-b" {
					gotSnap = true
				}
			}
		case <-deadline:
			t.Fatalf("switch path regressed: gotChanged=%v gotSnap=%v", gotChanged, gotSnap)
		}
	}
	// Destination on-start fired exactly once on the switch.
	waitForState(t, sb, incrCounterLeaf, "1", time.Second)
}

// TestReactivation_BootRefiresPersistedActive (criterion #12): at cold start,
// loadActiveScenes loads the persisted active scene via LoadExec then calls
// SetActive(persistedID) on a Show whose active pointer is still "". This test
// reproduces that exact runtime sequence (LoadExec -> SetActive from "") and
// asserts the persisted scene comes back on the antenna AND its on-start runs
// — covering the "scene inactive after restart" trap (live-testing.md) with a
// test rather than a manual post-deploy check. The DB-backed wiring of
// loadActiveScenes (store reads) is exercised by the e2e suite; here we pin
// the runtime contract Amendment 1 §A1.4 makes explicit.
func TestReactivation_BootRefiresPersistedActive(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)

	// Cold-start sequence: roster is filled, active pointer is empty.
	loadIncrScene(t, show, "scene-a")
	sa, _ := show.Get("scene-a")
	if show.ActiveID() != "" {
		t.Fatalf("fresh show active = %q, want empty before re-activation", show.ActiveID())
	}

	// loadActiveScenes' re-activation of the persisted pointer.
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	if show.ActiveID() != "scene-a" {
		t.Fatalf("after boot re-activation active = %q, want scene-a", show.ActiveID())
	}
	// The persisted active scene's on-start executed at cold start.
	waitForState(t, sa, incrCounterLeaf, "1", time.Second)
}
