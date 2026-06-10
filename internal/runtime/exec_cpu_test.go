package runtime

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Tests for the B7 per-scene-version CPU observability counter
// (`orion_task_cpu_seconds_total`, ADR 003 §3.1.6, issue #89): the
// exec layer accumulates each slice's elapsed time per scene_version,
// observes — and NEVER kills (doctrine §1.1) — and stays silent on a
// scene whose exec layer is dormant (R9).

// cpuLoopProg is a 0..last for-loop chain with an exact, assertable
// final state (the no-kill proof rides along). A large `last` makes
// one slice burn measurably more than the platform's monotonic-clock
// granularity (~0.5 ms on Windows), so the > 0 assertions hold on
// every CI runner.
func cpuLoopProg(last int) *ExecProgram {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"loop": {ID: "loop", Op: OpForLoop,
				Config: map[string]json.RawMessage{
					"first": raw(`0`),
					"last":  json.RawMessage(fmt.Sprintf("%d", last)),
				},
				Next: map[string]ExecTarget{
					"body":      {Node: "set"},
					"completed": {Node: "set.done"},
				}},
			"set": varSet("set", "counter",
				[]ExecDataInput{{Port: "value", From: "loop", FromPort: "index"}}, nil),
			"set.done": varSet("set.done", "loop_done", nil, nil),
		},
		Entrypoints: map[string]ExecEntry{"e": {Target: ExecTarget{Node: "loop"}}},
	}
	prog.Nodes["set.done"].Config["value"] = raw(`true`)
	return prog
}

// TestExecCPU_AccumulatesPerSceneVersion: running exec accumulates
// elapsed seconds on the metrics seam, keyed by the scene's
// scene_version — and the task still completes exactly (observe,
// never kill).
func TestExecCPU_AccumulatesPerSceneVersion(t *testing.T) {
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "cpu-test", cpuLoopProg(19_999))
	sc.SetExecMetrics(metrics)
	sc.SetExecSlicing(100_000_000, time.Hour) // one big slice: measurable elapsed
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.loop_done", `true`, 10*time.Second)
	waitForState(t, sc, "__vars.bp.counter", `19999`, 10*time.Second) // no kill: exact final state

	byVersion, calls := metrics.cpu()
	if calls < 1 {
		t.Fatal("no cpu report after a completed exec task")
	}
	if len(byVersion) != 1 {
		t.Fatalf("cpu accumulated under %d scene_versions, want exactly 1: %v", len(byVersion), byVersion)
	}
	got, ok := byVersion["sha256:exec-test"]
	if !ok {
		t.Fatalf("cpu not keyed by the scene's scene_version: %v", byVersion)
	}
	if got <= 0 {
		t.Fatalf("accumulated cpu seconds = %v, want > 0 (20k-iteration slice)", got)
	}
}

// TestExecCPU_ReportsEverySliceExit: under a tiny step budget the task
// is preempted and re-enqueued many times; EVERY slice exit —
// preemption and completion alike — reports once, so a long-running
// chain cannot hide between completions.
func TestExecCPU_ReportsEverySliceExit(t *testing.T) {
	metrics := &fakeExecMetrics{}
	sc := execScene(t, "cpu-slices", cpuLoopProg(9))
	sc.SetExecMetrics(metrics)
	sc.SetExecSlicing(3, time.Hour) // force step-budget preemption
	startScene(t, sc)

	mustFire(t, sc, "e")
	waitForState(t, sc, "__vars.bp.loop_done", `true`, time.Second)

	_, preempt, _ := metrics.counts()
	if preempt == 0 {
		t.Fatal("expected at least one preemption (3-step slices over a 10-iteration loop)")
	}
	byVersion, calls := metrics.cpu()
	if calls < preempt+1 {
		t.Fatalf("cpu reports = %d, want >= preemptions+completion = %d", calls, preempt+1)
	}
	if _, ok := byVersion["sha256:exec-test"]; !ok {
		t.Fatalf("cpu not keyed by the scene's scene_version: %v", byVersion)
	}
}

// TestExecCPU_TwoVersionsAccumulateSeparately: two scene instances on
// different scene_versions never share a CPU bucket — the per-version
// attribution is what lets an incident point at the pathological
// version, not the show.
func TestExecCPU_TwoVersionsAccumulateSeparately(t *testing.T) {
	metrics := &fakeExecMetrics{}

	mkScene := func(id, version string) *Scene {
		g := varsGraph(id)
		g.SceneVersion = version
		sc := NewScene(id, g, &compiler.RenderBundle{SceneVersion: version}, NewComputeRegistry(), quietLogger())
		sc.InstallExec(cpuLoopProg(19_999))
		sc.SetExecMetrics(metrics)
		sc.SetExecSlicing(100_000_000, time.Hour)
		startScene(t, sc)
		return sc
	}
	a := mkScene("cpu-a", "sha256:cpu-version-a")
	b := mkScene("cpu-b", "sha256:cpu-version-b")

	mustFire(t, a, "e")
	mustFire(t, b, "e")
	waitForState(t, a, "__vars.bp.loop_done", `true`, 10*time.Second)
	waitForState(t, b, "__vars.bp.loop_done", `true`, 10*time.Second)

	byVersion, _ := metrics.cpu()
	if len(byVersion) != 2 {
		t.Fatalf("cpu buckets = %v, want exactly the two scene_versions", byVersion)
	}
	for _, v := range []string{"sha256:cpu-version-a", "sha256:cpu-version-b"} {
		if byVersion[v] <= 0 {
			t.Fatalf("scene_version %s accumulated %v cpu seconds, want > 0", v, byVersion[v])
		}
	}
}

// TestExecCPU_SilentWithoutExec: a scene whose exec layer never runs
// (R9 — exec dormant in prod) reports NO cpu — the counter stays at
// its zero value, data-layer traffic notwithstanding.
func TestExecCPU_SilentWithoutExec(t *testing.T) {
	metrics := &fakeExecMetrics{}
	sc := NewScene("cpu-dormant", varsGraph("cpu-dormant"),
		&compiler.RenderBundle{SceneVersion: "sha256:exec-test"}, NewComputeRegistry(), quietLogger())
	sc.SetExecMetrics(metrics)
	startScene(t, sc)

	sc.Input(InputMsg{Path: "score.team_a", Value: raw(`14`), Source: "test"})
	waitForState(t, sc, "score.team_a", `14`, time.Second)

	if byVersion, calls := metrics.cpu(); calls != 0 || len(byVersion) != 0 {
		t.Fatalf("dormant exec reported cpu: calls=%d buckets=%v, want none", calls, byVersion)
	}
}
