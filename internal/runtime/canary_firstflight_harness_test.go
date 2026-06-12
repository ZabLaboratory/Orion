package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Canary first-flight DB-free harness (R9, issue #106). The full e2e
// push→validate→activate proof lives in tests/e2e/canary_firstflight_test.go
// and runs against a live PG in CI. This harness proves the SAME canary
// blueprint, WITHOUT a database, by driving the REAL compiler (so the
// artefact is exactly what POST /push would persist) and then running its
// exec live in a real Scene. It answers three questions locally:
//
//	1. does the canary payload COMPILE with zero error diagnostics?
//	2. does the compiled graph EMIT an ExecProgram with the expected
//	   entrypoints (on-start + on-tick)?
//	3. does on-start RUN LIVE → __vars.canary.counter == 9 and a print
//	   line lands in the __debug.canary.print ring?
//
// The blueprint here is byte-identical in shape to the runbook payload
// (docs/runbooks/canary-scene-payload.md). LIVE-ops-only: on-start,
// on-tick, for-loop, literal, variable.set/get, print, math.add — no
// http.request / db.query / source.read (Bastion condition).

func canaryHarnessBlueprint() *compiler.BlueprintGraph {
	ep := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
	}
	dp := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "any", Kind: "data"}
	}
	return &compiler.BlueprintGraph{
		ID: "bp-canary",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{ep("then")}},
			{ID: "first", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`0`)},
				Outputs: []compiler.BlueprintPort{dp("out")}},
			{ID: "last", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`9`)},
				Outputs: []compiler.BlueprintPort{dp("out")}},
			{ID: "loop", Compute: "core.flow.for-loop@1",
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("first"), dp("last")},
				Outputs: []compiler.BlueprintPort{ep("body"), ep("completed"), dp("index")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"counter"`)},
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("value")},
				Outputs: []compiler.BlueprintPort{ep("then")}},
			{ID: "doneMsg", Compute: "core.literal@1",
				Config: map[string]json.RawMessage{
					"value": json.RawMessage(`"canary first-flight: maintainers loop complete (counter=9)"`)},
				Outputs: []compiler.BlueprintPort{dp("out")}},
			{ID: "print", Compute: "core.print@1",
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("value")},
				Outputs: []compiler.BlueprintPort{ep("then")}},
			{ID: "tick", Compute: "core.event.on-tick@1",
				Outputs: []compiler.BlueprintPort{ep("then"), dp("delta_seconds")}},
			{ID: "ticksGet", Compute: "core.variable.get@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"ticks"`)},
				Outputs: []compiler.BlueprintPort{dp("out")}},
			{ID: "one", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`1`)},
				Outputs: []compiler.BlueprintPort{dp("out")}},
			{ID: "ticksAdd", Compute: "core.math.add@1",
				Inputs:  []compiler.BlueprintPort{dp("a"), dp("b")},
				Outputs: []compiler.BlueprintPort{dp("out")}},
			{ID: "ticksSet", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"ticks"`)},
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("value")},
				Outputs: []compiler.BlueprintPort{ep("then")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "loop", ToPort: "exec_in"},
			{FromNode: "first", FromPort: "out", ToNode: "loop", ToPort: "first"},
			{FromNode: "last", FromPort: "out", ToNode: "loop", ToPort: "last"},
			{FromNode: "loop", FromPort: "body", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "loop", FromPort: "index", ToNode: "set", ToPort: "value"},
			{FromNode: "loop", FromPort: "completed", ToNode: "print", ToPort: "exec_in"},
			{FromNode: "doneMsg", FromPort: "out", ToNode: "print", ToPort: "value"},
			{FromNode: "tick", FromPort: "then", ToNode: "ticksSet", ToPort: "exec_in"},
			{FromNode: "ticksGet", FromPort: "out", ToNode: "ticksAdd", ToPort: "a"},
			{FromNode: "one", FromPort: "out", ToNode: "ticksAdd", ToPort: "b"},
			{FromNode: "ticksAdd", FromPort: "out", ToNode: "ticksSet", ToPort: "value"},
		},
	}
}

func canaryHarnessFetcher() *stubFetcher {
	return &stubFetcher{
		layout:    &compiler.CanvasLayout{Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		blueprint: canaryHarnessBlueprint(),
		manifest: compiler.ComputeManifest{
			"core.event.on-start@1": {IsPure: true, IsBounded: true, Version: "1"},
			"core.event.on-tick@1":  {IsPure: true, IsBounded: true, Version: "1"},
			"core.literal@1":        {IsPure: true, IsBounded: true, Version: "1"},
			"core.flow.for-loop@1":  {IsPure: true, IsBounded: true, Version: "1"},
			"core.variable.set@1":   {IsPure: true, IsBounded: true, Version: "1"},
			"core.variable.get@1":   {IsPure: true, IsBounded: true, Version: "1"},
			"core.print@1":          {IsPure: true, IsBounded: true, Version: "1"},
			"core.math.add@1":       {IsPure: true, IsBounded: true, Version: "1"},
		},
	}
}

// TestCanary_CompilesEmitsExecAndRunsLive is the DB-free flight proof.
func TestCanary_CompilesEmitsExecAndRunsLive(t *testing.T) {
	// 1) COMPILE — exactly the artefact POST /push would persist.
	graph, bundle, version, err := compiler.Compile(context.Background(), "canary-r9-firstflight",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-canary"},
		canaryHarnessFetcher())
	if err != nil {
		t.Fatalf("canary payload failed to compile: %v", err)
	}
	if version == "" {
		t.Fatal("compile minted an empty scene_version")
	}

	// 2) EMIT — the compiled graph yields an ExecProgram with on-start AND
	// on-tick entrypoints (the franchissement carries live triggers).
	progs, err := ExecProgramsFromGraph(graph)
	if err != nil {
		t.Fatalf("canary graph did not emit an ExecProgram: %v", err)
	}
	if len(progs) == 0 {
		t.Fatal("canary graph emitted zero exec programs")
	}
	var sawOnStart, sawOnTick bool
	for _, p := range progs {
		for _, e := range p.Entrypoints {
			switch e.Kind {
			case EntryOnStart:
				sawOnStart = true
			case EntryOnTick:
				sawOnTick = true
			}
		}
	}
	if !sawOnStart {
		t.Fatal("canary exec program is missing the on-start entrypoint")
	}
	if !sawOnTick {
		t.Fatal("canary exec program is missing the on-tick entrypoint (air-only trigger)")
	}

	// 3) RUN LIVE — install the program, fire on-start, observe the
	// maintainer's loop reach counter==9 and the print land in __debug.
	sc := NewScene("canary-r9-firstflight", graph, bundle, NewComputeRegistry(), quietLogger())
	for _, p := range progs {
		sc.InstallExec(p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sc.Run(ctx)
	t.Cleanup(sc.Stop)

	// Resolve the on-start entry name and fire it (mirrors what going on
	// air does for the live scene).
	startEntry := ""
	for _, p := range progs {
		for name, e := range p.Entrypoints {
			if e.Kind == EntryOnStart {
				startEntry = name
			}
		}
	}
	if startEntry == "" {
		t.Fatal("no resolvable on-start entry name")
	}
	if !sc.FireExec(startEntry, "canary-harness") {
		t.Fatal("inbox full firing on-start")
	}

	counterKey := canaryLeafKey(progs, "counter")
	waitForState(t, sc, counterKey, `9`, 3*time.Second)

	// The print line landed in the __debug ring (observable in snapshot).
	debugKey := canaryDebugKey(progs)
	deadline := time.Now().Add(3 * time.Second)
	printed := false
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get(debugKey); ok && strings.Contains(string(v), "canary") {
			printed = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !printed {
		v, _ := sc.state.Get(debugKey)
		t.Fatalf("print never landed in %s; last = %q", debugKey, v)
	}

	// on-tick fires live: this scene is on air (a NewScene roster instance
	// is not triggersGated, so its on-tick chain fires on the tick leaf),
	// so injecting the global tick the runtime's ticker would fan out makes
	// the ticks counter climb — proving the on-tick → variable.get →
	// math.add → variable.set chain runs live.
	for i := 0; i < 4; i++ {
		sc.Input(InputMsg{
			Path:     "__system.tick.now_ms",
			Value:    json.RawMessage("1"),
			Source:   "system:tick",
			IsSystem: true,
		})
	}
	// HARDENED (variable-get cross-tick bug): require the counter to reach
	// EXACTLY 4 — one increment per tick across 4 ticks. The old assertion
	// (`!= "0"`) passed even when the counter froze at 1, masking the bug
	// where variable.get read a never-written leaf (0) every tick so
	// add(0,1) produced a constant 1. A real cross-tick read makes the chain
	// on-tick → get(ticks) → add(+1) → set(ticks) climb 1,2,3,4.
	ticksKey := canaryLeafKey(progs, "ticks")
	waitForState(t, sc, ticksKey, `4`, 3*time.Second)
}

// canaryLeafKey returns __vars.<key>.<name> for the (single) blueprint key
// the compiler assigned — the legacy single-blueprint push uses key "".
func canaryLeafKey(progs []*ExecProgram, name string) string {
	return "__vars." + progs[0].BlueprintKey + "." + name
}

func canaryDebugKey(progs []*ExecProgram) string {
	return "__debug." + progs[0].BlueprintKey + ".print"
}
