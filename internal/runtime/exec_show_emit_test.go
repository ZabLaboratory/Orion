package runtime

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// `show.emit` executor + active-only injection + anti-loop (ADR 009 §3.6,
// issue #155, resolution criterion #4).
//
// These tests prove the executor reads config `topic` + input `payload`,
// injects `__events.<topic>` to the ACTIVE scene ONLY (via the Show's emit
// seam), fires `then`, and — critically — that a rule's emission NEVER
// cascades to other promoted rules (the asymmetry is the anti-loop
// invariant: emit is active-only, NOT the §3.3 RouteTargets union).
//
// The injection sink in production is adapters.Inbox.EmitToActive (which
// also audits — proven in internal/adapters/inbox_stream_rule_test.go). The
// runtime package cannot import adapters (cycle), so these tests install a
// faithful in-package double that performs the SAME active-only delivery:
// write a system __events.<topic> to show.Active() alone. The double is the
// runtime-package contract for what any Emitter must do.

// activeOnlyEmitter is a runtime.Emitter test double mirroring the
// production injection: it delivers a system `__events.<topic>` write to
// show.Active() ONLY — never RouteTargets, so no rule→rule cascade. It
// records every delivery target id for the no-cascade assertion.
type activeOnlyEmitter struct {
	show *Show
}

func (e *activeOnlyEmitter) EmitToActive(topic string, payload json.RawMessage) {
	active := e.show.Active()
	if active == nil {
		return
	}
	active.Input(InputMsg{
		Path:     eventsPrefix + topic,
		Value:    payload,
		Source:   "system:show.emit",
		IsSystem: true,
	})
}

// emitOnStartProg fires `on-start` → show.emit(topic) → variable.set marking
// `__vars.bp.emitted = 1`. The on-start fires once at promotion (rules) or
// activation (active scene).
func emitOnStartProg(topic string) *ExecProgram {
	mark := varSet("emitted", "emitted", nil, nil)
	mark.Config["value"] = raw(`1`)
	emit := &ExecNode{
		ID:     "emit",
		Op:     OpShowEmit,
		Config: map[string]json.RawMessage{"topic": raw(`"` + topic + `"`), "payload": raw(`{"k":"v"}`)},
		Next:   map[string]ExecTarget{"then": {Node: "emitted"}},
	}
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes:        map[string]*ExecNode{"emit": emit, "emitted": mark},
		Entrypoints: map[string]ExecEntry{
			"start": {Target: ExecTarget{Node: "emit"}, Kind: EntryOnStart},
		},
	}
}

// onEventSetProg fires `on-event <topic>` → variable.set `__vars.bp.hit =
// value`. A distinct value per listener lets a test tell which one fired.
func onEventSetProg(topic string, value int) *ExecProgram {
	set := varSet("hit", "hit", nil, nil)
	set.Config["value"] = raw(strconv.Itoa(value))
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes:        map[string]*ExecNode{"hit": set},
		Entrypoints: map[string]ExecEntry{
			"on" + topic: {Target: ExecTarget{Node: "hit"}, Kind: EntryOnEvent, Event: topic},
		},
	}
}

// TestExec_OnEvent_PayloadBoundToFiredTask (live finale null-text regression,
// twin of TestExec_OnPlatformEvent_PayloadBoundToFiredTask): firing an
// on-event entry on a `__events.<topic>` write must bind the TRIGGERING EVENT
// VALUE under the entry node's `payload` data-out pin — exactly as
// on-platform-event binds its leaf and on-tick binds `delta_seconds`. Before
// the fix the branch fired with no env, so a downstream `payload` read
// resolved to null (the on-event node is an exec node, not a dataflow node —
// demandValue finds no state leaf at `<node>`). This is the latent gap behind
// the finale `show.emit → on-event(payload) → get-field(payload.text)` path:
// on-platform-event was fixed (#173) but on-event was left unbound. Here a
// spine `on-event(topic) → set` whose value pulls `<entry>.payload` must write
// the FULL event value EmitToActive wrote at `__events.<topic>`, not null.
func TestExec_OnEvent_PayloadBoundToFiredTask(t *testing.T) {
	topic := "alert"
	canonical := `{"type":"chat","payload":{"text":"hello"}}`

	// on-event(topic) → set `__vars.bp.received = <onev>.payload`. The entry
	// carries Node so the runtime knows which node namespaces the payload pin
	// (the compiler sets ExecEntry.Node = the event node's id).
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "received",
				[]ExecDataInput{{Port: "value", From: "onev", FromPort: "payload"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"onev": {Kind: EntryOnEvent, Event: topic, Node: "onev",
				Target: ExecTarget{Node: "set"}},
		},
	}
	sc := execScene(t, "event-payload", prog)
	startScene(t, sc)

	// A system write of the canonical event to `__events.<topic>` (what
	// EmitToActive does) fires the spine; the fired task must observe the
	// event VALUE on its `payload` pin and land it verbatim — proving the
	// payload is no longer null at the source.
	sc.Input(InputMsg{Path: eventsPrefix + topic, Value: raw(canonical), Source: "system:show.emit", IsSystem: true})
	waitForState(t, sc, "__vars.bp.received", canonical, time.Second)

	// A SECOND write with a different payload re-fires and lands the new
	// value (fire-on-write carries the current event, not a stale binding).
	next := `{"type":"chat","payload":{"text":"world"}}`
	sc.Input(InputMsg{Path: eventsPrefix + topic, Value: raw(next), Source: "system:show.emit", IsSystem: true})
	waitForState(t, sc, "__vars.bp.received", next, time.Second)
}

// TestShowEmit_RuleToActiveNoCascade (criterion #4, the anti-loop proof,
// referenced by the conformance matrix for core.show.emit@1):
//
//   - an emit-source RULE emits `__events.alert` on promotion;
//   - the ACTIVE scene's on-event `alert` entry fires (hit = 7);
//   - ANOTHER promoted rule that ALSO listens to `alert` does NOT fire
//     (its hit stays at the default 0) — emit is active-only, never
//     rule→rule, so no cascade and no loop.
func TestShowEmit_RuleToActiveNoCascade(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.SetEmitter(&activeOnlyEmitter{show: show})
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	// Active scene listens to `alert` → hit = 7.
	show.LoadExec("scene-active", varsGraph("scene-active"), bundle, onEventSetProg("alert", 7))
	if err := show.SetActive("scene-active", nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	// A listener RULE also listens to `alert` → hit = 9. It must NOT fire
	// when the emit comes from another rule (no cascade).
	if err := show.PromoteStreamRule("rule-listener", varsGraph("rule-listener"), bundle, onEventSetProg("alert", 9)); err != nil {
		t.Fatalf("promote listener: %v", err)
	}
	// The emit-source RULE emits `alert` on its promotion on-start fire.
	if err := show.PromoteStreamRule("rule-emitter", varsGraph("rule-emitter"), bundle, emitOnStartProg("alert")); err != nil {
		t.Fatalf("promote emitter: %v", err)
	}

	active, _ := show.Get("scene-active")
	listener, _ := show.Get("rule-listener")
	emitter, _ := show.Get("rule-emitter")

	// The emitter ran its show.emit and fell through to `then` (emitted=1).
	waitForState(t, emitter, "__vars.bp.emitted", "1", 2*time.Second)
	// The ACTIVE scene received the event and fired its on-event (hit=7).
	waitForState(t, active, "__vars.bp.hit", "7", 2*time.Second)

	// The OTHER rule must NOT have fired — emit is active-only, no cascade.
	// Give it a generous grace window; the value must stay the default 0.
	time.Sleep(200 * time.Millisecond)
	if v, ok := listener.state.Get("__vars.bp.hit"); ok && string(v) != "0" {
		t.Fatalf("listener rule fired on a rule's emit (hit=%s) — emit cascaded rule→rule", v)
	}
}

// TestShowEmit_FiresThenWithoutActiveScene: with NO active scene, show.emit
// is absorbed (no target) but STILL fires `then` — construction-safe, no
// error pin (Blue#73). The chain reaches `emitted=1`.
func TestShowEmit_FiresThenWithoutActiveScene(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	show.SetEmitter(&activeOnlyEmitter{show: show})
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	if err := show.PromoteStreamRule("rule-emitter", varsGraph("rule-emitter"), bundle, emitOnStartProg("alert")); err != nil {
		t.Fatalf("promote emitter: %v", err)
	}
	emitter, _ := show.Get("rule-emitter")
	// No active scene; the emit drops, `then` still fires.
	waitForState(t, emitter, "__vars.bp.emitted", "1", 2*time.Second)
}

// TestShowEmit_NilSeamConstructionSafe: an unwired emit seam (no emitter set)
// never halts the chain — `then` fires, the injection is a no-op.
func TestShowEmit_NilSeamConstructionSafe(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	// No SetEmitter call → sh.emitter nil → emitToActive no-ops.
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	if err := show.PromoteStreamRule("rule-emitter", varsGraph("rule-emitter"), bundle, emitOnStartProg("alert")); err != nil {
		t.Fatalf("promote emitter: %v", err)
	}
	emitter, _ := show.Get("rule-emitter")
	waitForState(t, emitter, "__vars.bp.emitted", "1", 2*time.Second)
}
