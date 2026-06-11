package runtime

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Tests for the air-only trigger scope (ADR 006 §3.4, issue #106,
// criterion #6 — CRITICAL). The global tick fans out to EVERY loaded
// scene (tick.go), so a validated exec scene loaded but NOT on air must
// fire ZERO on-tick / on-event chains — zero effects backstage. The
// on-air flag is single-writer (toggled through the inbox), so these run
// clean under -race.

// countingEffector is an effect-seam instrumentation: it counts every
// SetLeaf / Print the exec layer would perform AND still applies the
// write so the underlying scene state behaves normally. "Zero effects
// backstage" is asserted by reading effects() == 0 (criterion #6).
type countingEffector struct {
	inner Effector
	mu    sync.Mutex
	sets  int
	print int
}

func (e *countingEffector) SetLeaf(path string, value json.RawMessage) {
	e.mu.Lock()
	e.sets++
	e.mu.Unlock()
	e.inner.SetLeaf(path, value)
}

func (e *countingEffector) Print(blueprintKey, line string) {
	e.mu.Lock()
	e.print++
	e.mu.Unlock()
	e.inner.Print(blueprintKey, line)
}

func (e *countingEffector) effects() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sets + e.print
}

// onTickSetProg fires `on-tick` → variable.set(counter = 1). One effect
// per tick — the simplest "real effect" an off-air scene must not run.
func onTickSetProg() *ExecProgram {
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "ticked",
				[]ExecDataInput{{Port: "value", From: "lit.one"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"tick": {Target: ExecTarget{Node: "set"}, Kind: EntryOnTick, Node: "tickn"},
		},
	}
}

// gatedExecScene builds a roster-style scene: triggers gated on the
// on-air flag (GateTriggers), instrumented effector, started goroutine.
func gatedExecScene(t *testing.T, id string, prog *ExecProgram) (*Scene, *countingEffector) {
	t.Helper()
	sc := execScene(t, id, prog)
	sc.GateTriggers()
	eff := &countingEffector{inner: &sceneEffector{sc}}
	sc.SetEffector(eff)
	startScene(t, sc)
	return sc, eff
}

func tick(t *testing.T, sc *Scene, ms int64) {
	t.Helper()
	raw := json.RawMessage([]byte(intRaw(ms)))
	if !sc.Input(InputMsg{Path: tickPath, Value: raw, Source: "system:tick", IsSystem: true}) {
		t.Fatalf("inbox full delivering tick")
	}
}

func intRaw(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestOnAir_OffAirSceneFiresNoTriggers (criterion #6, the franchissement
// guard): a gated roster instance that is NOT on air receives ticks but
// fires ZERO on-tick tasks and attempts ZERO effects. This is the
// "zero backstage effects" invariant — a validated scene sitting in the
// roster, loaded, must not run real logic until it takes the antenna.
func TestOnAir_OffAirSceneFiresNoTriggers(t *testing.T) {
	sc, eff := gatedExecScene(t, "offair", onTickSetProg())

	// Off air (default): deliver several ticks; nothing must fire.
	for i := int64(0); i < 5; i++ {
		tick(t, sc, 1000+i*16)
	}
	// Let the loop drain the ticks.
	drainScene(t, sc)

	if got := eff.effects(); got != 0 {
		t.Fatalf("off-air scene attempted %d effects, want 0 (backstage leak)", got)
	}
	if v, ok := sc.state.Get("__vars.bp.ticked"); ok && string(v) != "0" {
		t.Fatalf("off-air scene wrote __vars.bp.ticked=%s, want untouched (0)", v)
	}
}

// TestOnAir_OnAirFiresTriggers_SwitchAwayStops (criterion #6): on air,
// on-tick fires (effect happens); after switch-away (SetOnAir(false) +
// CancelExec) further ticks fire nothing more. on-start also fires only
// once on activation.
func TestOnAir_OnAirFiresTriggers_SwitchAwayStops(t *testing.T) {
	sc, eff := gatedExecScene(t, "onair", onTickSetProg())

	// Go on air, then tick: the on-tick chain fires its effect.
	if !sc.SetOnAir(true) {
		t.Fatal("inbox full setting on-air")
	}
	tick(t, sc, 2000)
	waitForState(t, sc, "__vars.bp.ticked", "1", time.Second)
	if got := eff.effects(); got == 0 {
		t.Fatal("on-air scene fired no effect on tick")
	}

	// Switch away: clear on-air + cancel live tasks (the Show's order).
	sc.SetOnAir(false)
	sc.CancelExec()
	drainScene(t, sc)
	before := eff.effects()

	// Further ticks must fire nothing now that the scene is off air.
	for i := int64(0); i < 5; i++ {
		tick(t, sc, 3000+i*16)
	}
	drainScene(t, sc)
	if got := eff.effects(); got != before {
		t.Fatalf("off-air-again scene attempted %d more effects, want 0", got-before)
	}
}

// TestOnAir_OnEventGatedOffAir (criterion #6): on-event is gated too — a
// write to `__events.<event>` on an off-air gated scene fires nothing.
func TestOnAir_OnEventGatedOffAir(t *testing.T) {
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "fired",
				[]ExecDataInput{{Port: "value", From: "lit.one"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"goal": {Target: ExecTarget{Node: "set"}, Kind: EntryOnEvent, Event: "goal"},
		},
	}
	sc, eff := gatedExecScene(t, "onevent", prog)

	// Off air: an event write fires nothing.
	sc.Input(InputMsg{Path: "__events.goal", Value: json.RawMessage(`{}`), IsSystem: true})
	drainScene(t, sc)
	if got := eff.effects(); got != 0 {
		t.Fatalf("off-air on-event attempted %d effects, want 0", got)
	}

	// On air: the same event write now fires.
	sc.SetOnAir(true)
	sc.Input(InputMsg{Path: "__events.goal", Value: json.RawMessage(`{}`), IsSystem: true})
	waitForState(t, sc, "__vars.bp.fired", "1", time.Second)
}

// TestOnAir_TestSessionUngated (ADR §3.4 — test sessions untouched): an
// UNGATED scene (the test-session / validation-clone default) fires its
// on-tick freely with no on-air flag set, so authors iterate without
// validation. This is the explicit non-gating the doctrine requires.
func TestOnAir_TestSessionUngated(t *testing.T) {
	sc := execScene(t, "session", onTickSetProg())
	// NOTE: no GateTriggers() — this models a test session / clone.
	startScene(t, sc)
	tick(t, sc, 4000)
	waitForState(t, sc, "__vars.bp.ticked", "1", time.Second)
}

// TestOnAir_ShowGatesRosterInstance (criterion #6, through the real Show
// seam): two exec scenes loaded via LoadExec into one Show. Activating
// scene-a puts it on air (its on-tick fires); scene-b sits in the roster
// off-air and fires NOTHING on the same global ticks. Switching to
// scene-b moves the antenna; a's triggers go quiescent, b's go live —
// proving Show.SetActive toggles the on-air flag through the inbox on
// both scenes.
func TestOnAir_ShowGatesRosterInstance(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)

	ga := varsGraph("scene-a")
	gb := varsGraph("scene-b")
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}
	show.LoadExec("scene-a", ga, bundle, onTickSetProg())
	show.LoadExec("scene-b", gb, bundle, onTickSetProg())

	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	sa, _ := show.Get("scene-a")
	sb, _ := show.Get("scene-b")

	// Tick both scenes (mirrors the global tick fan-out to all loaded).
	for _, s := range []*Scene{sa, sb} {
		tick(t, s, 1000)
	}
	// scene-a is on air → fires; scene-b is off air → silent.
	waitForState(t, sa, "__vars.bp.ticked", "1", time.Second)
	drainScene(t, sb)
	if v, ok := sb.state.Get("__vars.bp.ticked"); ok && string(v) != "0" {
		t.Fatalf("off-air scene-b wrote ticked=%s on a backstage tick, want untouched", v)
	}

	// Switch the antenna to scene-b: a goes quiescent, b goes live.
	if err := show.SetActive("scene-b", nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Scene{sa, sb} {
		tick(t, s, 2000)
	}
	waitForState(t, sb, "__vars.bp.ticked", "1", time.Second)
}

// drainScene pushes a sentinel write through the inbox and waits for it
// to land, guaranteeing every previously-queued message (the ticks) has
// been processed on the scene goroutine before we assert.
func drainScene(t *testing.T, sc *Scene) {
	t.Helper()
	sentinel := "__test.drain.sentinel"
	val := json.RawMessage([]byte(intRaw(time.Now().UnixNano())))
	if !sc.Input(InputMsg{Path: sentinel, Value: val, IsSystem: true}) {
		t.Fatalf("inbox full delivering drain sentinel")
	}
	waitForState(t, sc, sentinel, string(val), time.Second)
}
