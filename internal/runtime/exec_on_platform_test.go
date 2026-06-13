package runtime

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// on-platform-event firing (ADR 013 §3 / §6, criteria #3 and #4).
//
// The arming twin of on-event: an entry of Kind EntryOnPlatformEvent carries
// the FULL `__inputs.platform.*` leaf in its Event field and is indexed by
// that leaf in execOnPlatform. A WRITE to that leaf fires the spine — on the
// WRITE, not the value change (a payload written twice is two events), one
// fire per write per observing entry. These tests drive a real Scene.

const platformChatLeaf = "__inputs.platform.twitch.zab.last_chat"

// onPlatformPrintProg fires `on-platform-event <leaf>` → print(msg). The
// append-ordered __debug ring is the deterministic fire counter.
func onPlatformPrintProg(key, leaf, msg string) *ExecProgram {
	return &ExecProgram{
		BlueprintKey: key,
		Nodes: map[string]*ExecNode{
			"p": {ID: "p", Op: OpPrint,
				Config: map[string]json.RawMessage{"value": raw(`"` + msg + `"`)}},
		},
		Entrypoints: map[string]ExecEntry{
			"onplat": {Kind: EntryOnPlatformEvent, Event: leaf, Target: ExecTarget{Node: "p"}},
		},
	}
}

// TestExec_OnPlatformEvent_FiresPerWrite (criterion #3): a write to the
// observed platform leaf fires the spine exactly once per write — and a
// SECOND write with the SAME payload fires AGAIN (fire-on-write, not
// on-change). A write to a DIFFERENT platform leaf the entry does not
// observe never fires it.
func TestExec_OnPlatformEvent_FiresPerWrite(t *testing.T) {
	sc := multiScene(t, "platform", onPlatformPrintProg("bp", platformChatLeaf, "hit"))

	// Indexed by the full leaf (NOT a shortened topic, unlike on-event).
	if got := sc.execOnPlatform[platformChatLeaf]; !reflect.DeepEqual(got, []string{"bp/onplat"}) {
		t.Fatalf("execOnPlatform[%q] = %v, want [\"bp/onplat\"]", platformChatLeaf, got)
	}

	startScene(t, sc)

	// First write fires once.
	sc.Input(InputMsg{Path: platformChatLeaf, Value: raw(`{"user":"a","text":"yo"}`)})
	waitForState(t, sc, "__debug.bp.print", `["hit"]`, time.Second)

	// Second write with the IDENTICAL payload fires AGAIN — fire-on-write,
	// not on value change (the on-event invariant, shared here).
	sc.Input(InputMsg{Path: platformChatLeaf, Value: raw(`{"user":"a","text":"yo"}`)})
	waitForState(t, sc, "__debug.bp.print", `["hit","hit"]`, time.Second)

	// A write to a platform leaf the entry does NOT observe never fires it.
	sc.Input(InputMsg{Path: "__inputs.platform.twitch.zab.last_follow", Value: raw(`{"user":"b"}`)})
	time.Sleep(150 * time.Millisecond)
	if v, _ := sc.state.Get("__debug.bp.print"); string(v) != `["hit","hit"]` {
		t.Fatalf("entry fired on an unobserved leaf: ring=%s", v)
	}
}

// TestExec_OnPlatformEvent_RoutesByLeaf (criterion #4): the merged
// execOnPlatform index routes a platform write to every entry observing that
// exact leaf — and ONLY those. bpA observes last_chat, bpB observes
// last_follow; each fires independently under its namespaced key.
func TestExec_OnPlatformEvent_RoutesByLeaf(t *testing.T) {
	chatLeaf := "__inputs.platform.twitch.zab.last_chat"
	followLeaf := "__inputs.platform.twitch.zab.last_follow"
	progA := onPlatformVarProg("bpA", chatLeaf, 1)
	progB := onPlatformVarProg("bpB", followLeaf, 2)

	sc := multiScene(t, "platform-routes", progA, progB)
	startScene(t, sc)

	// A chat write fires bpA only.
	sc.Input(InputMsg{Path: chatLeaf, Value: raw(`{"user":"x"}`)})
	waitForState(t, sc, "__vars.bpA.counter", `1`, time.Second)
	if v, _ := sc.state.Get("__vars.bpB.counter"); string(v) != `0` {
		t.Fatalf("bpB fired on a chat leaf it does not observe: %s", v)
	}

	// A follow write fires bpB only.
	sc.Input(InputMsg{Path: followLeaf, Value: raw(`{"user":"y"}`)})
	waitForState(t, sc, "__vars.bpB.counter", `2`, time.Second)
}

// onPlatformVarProg fires `on-platform-event <leaf>` → variable.set
// `__vars.<key>.counter = value`.
func onPlatformVarProg(key, leaf string, value int) *ExecProgram {
	n := varSet("set", "counter", nil, nil)
	n.Config["value"] = raw(strconv.Itoa(value))
	return &ExecProgram{
		BlueprintKey: key,
		Nodes:        map[string]*ExecNode{"set": n},
		Entrypoints: map[string]ExecEntry{
			"onplat": {Kind: EntryOnPlatformEvent, Event: leaf, Target: ExecTarget{Node: "set"}},
		},
	}
}

// TestExec_OnPlatformEvent_PayloadBoundToFiredTask (ADR 013, live finale
// null-text regression): firing on a platform write must bind the TRIGGERING
// LEAF VALUE under the entry node's `payload` data-out pin, exactly as on-tick
// binds `delta_seconds` and on-event surfaces its `payload`. Before the fix
// the branch fired with no env, so a downstream `payload` read resolved to
// null (the on-platform node is an exec node, not a dataflow node — demandValue
// finds no state leaf at `<node>`). Here a spine `on-platform-event → set` whose
// value pulls `<entry>.payload` must write the FULL canonical event value, not
// null. msg.Value is the canonical `{type, payload:{...}}` Quasar writes.
func TestExec_OnPlatformEvent_PayloadBoundToFiredTask(t *testing.T) {
	leaf := "__inputs.platform.twitch.g2nmathias.last_chat"
	canonical := `{"type":"chat","payload":{"text":"hello"}}`

	// on-platform-event(leaf) → set `__vars.bp.received = <onplat>.payload`.
	// The entry carries Node so the runtime knows which node namespaces the
	// payload pin (the compiler sets ExecEntry.Node = the event node's id).
	prog := &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"set": varSet("set", "received",
				[]ExecDataInput{{Port: "value", From: "onplat", FromPort: "payload"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"onplat": {Kind: EntryOnPlatformEvent, Event: leaf, Node: "onplat",
				Target: ExecTarget{Node: "set"}},
		},
	}
	sc := execScene(t, "platform-payload", prog)
	startScene(t, sc)

	// A write of the canonical event fires the spine; the fired task must
	// observe the LEAF VALUE on its `payload` pin and land it verbatim —
	// proving the payload is no longer null at the source.
	sc.Input(InputMsg{Path: leaf, Value: raw(canonical)})
	waitForState(t, sc, "__vars.bp.received", canonical, time.Second)

	// A SECOND write with a different payload re-fires and lands the new
	// value (fire-on-write carries the current leaf, not a stale binding).
	next := `{"type":"chat","payload":{"text":"world"}}`
	sc.Input(InputMsg{Path: leaf, Value: raw(next)})
	waitForState(t, sc, "__vars.bp.received", next, time.Second)
}

// TestExec_OnPlatformEvent_CoexistsWithDataflow (criterion #4, coexistence):
// a scene carrying BOTH a dataflow leaf binding (the quasar.* reactive value
// path) AND an on-platform-event entry on the SAME leaf sees BOTH on one
// write — the dataflow recompute lands the value on a downstream output AND
// the exec spine fires. Proven on one Scene: the platform write seeds the
// leaf (consumed by a passthrough output node) and arms the entry.
func TestExec_OnPlatformEvent_CoexistsWithDataflow(t *testing.T) {
	leaf := "__inputs.platform.twitch.zab.last_chat"
	// Graph: the platform leaf feeds an output (the dataflow/M9 path),
	// plus a __vars counter the exec spine writes.
	graph := &compiler.Graph{
		SceneID:      "coexist",
		SceneVersion: "sha256:multi-exec-test",
		Nodes: []compiler.GraphNode{
			{ID: "in", Kind: "input", Path: leaf},
			{ID: "out", Kind: "output", Compute: "core.output@1", Path: "out.value",
				Upstream: []string{"in"},
				Inputs:   []compiler.GraphInput{{From: "in", Port: "value"}}},
			{ID: "var", Kind: "input", Path: "__vars.bp.counter"},
		},
		Defaults: map[string]json.RawMessage{"__vars.bp.counter": raw(`0`)},
	}
	sc := NewScene("coexist", graph, &compiler.RenderBundle{SceneVersion: "sha256:multi-exec-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(onPlatformVarProg("bp", leaf, 5))
	startScene(t, sc)

	sc.Input(InputMsg{Path: leaf, Value: raw(`{"user":"x","text":"hello"}`)})

	// The exec spine fired (counter = 5)...
	waitForState(t, sc, "__vars.bp.counter", `5`, time.Second)
	// ...AND the dataflow recompute landed the value on the output leaf.
	waitForState(t, sc, "out.value", `{"user":"x","text":"hello"}`, time.Second)
}
