package runtime

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Tests for the multi-program install seam (ADR 006 §3.3, issue #105):
// one live scene hosts ALL of its blueprints' ExecPrograms. The trigger
// indexes merge under namespaced keys `<blueprint_key>/<entry_id>`,
// deterministically sorted; `__vars.<blueprint_key>.*` stays disjoint
// per blueprint (two blueprints of one scene) AND per instance (two
// instances sharing a blueprint_key) — B9 intact.

// multiVarsGraph exposes the `__vars.<key>.*` leaves the two test
// blueprints read/write so the data layer resolves their defaults.
func multiVarsGraph(id string) *compiler.Graph {
	return &compiler.Graph{
		SceneID:      id,
		SceneVersion: "sha256:multi-exec-test",
		Nodes: []compiler.GraphNode{
			{ID: "var.a", Kind: "input", Path: "__vars.bpA.counter"},
			{ID: "var.b", Kind: "input", Path: "__vars.bpB.counter"},
		},
		Defaults: map[string]json.RawMessage{
			"__vars.bpA.counter": raw(`0`),
			"__vars.bpB.counter": raw(`0`),
		},
	}
}

// setProg builds a one-blueprint program whose `on-start` writes
// `__vars.<key>.counter = value`. Both test blueprints share the
// entrypoint id "start" and node id "set" on purpose — the namespacing
// is what keeps them from colliding when merged onto one scene.
func setProg(key string, value int) *ExecProgram {
	n := varSet("set", "counter", nil, nil)
	n.Config["value"] = raw(strconv.Itoa(value))
	return &ExecProgram{
		BlueprintKey: key,
		Nodes:        map[string]*ExecNode{"set": n},
		Entrypoints:  map[string]ExecEntry{"start": {Kind: EntryOnStart, Target: ExecTarget{Node: "set"}}},
	}
}

func multiScene(t *testing.T, id string, progs ...*ExecProgram) *Scene {
	t.Helper()
	sc := NewScene(id, multiVarsGraph(id), &compiler.RenderBundle{SceneVersion: "sha256:multi-exec-test"}, NewComputeRegistry(), quietLogger())
	sc.InstallExec(progs...)
	return sc
}

// TestExecMulti_TwoBlueprintsInstalledOnOneInstance (criterion #4): a
// 2-blueprint exec scene installs BOTH programs on ONE instance; both
// `on-start` entries fire on activation and each writes its OWN
// blueprint-namespaced var — disjoint (B9), proven on the single scene.
func TestExecMulti_TwoBlueprintsInstalledOnOneInstance(t *testing.T) {
	progA := setProg("bpA", 7)
	progB := setProg("bpB", 9)

	sc := multiScene(t, "multi", progA, progB)

	// Both programs are hosted; the merged on-start index carries BOTH
	// entries, namespaced and sorted (determinism).
	wantOnStart := []string{"bpA/start", "bpB/start"}
	if !reflect.DeepEqual(sc.execOnStart, wantOnStart) {
		t.Fatalf("merged on-start index = %v, want %v (namespaced + sorted)", sc.execOnStart, wantOnStart)
	}
	if len(sc.execProgs) != 2 || sc.execProgs["bpA"] != progA || sc.execProgs["bpB"] != progB {
		t.Fatalf("both programs must be hosted on the one instance: %v", sc.execProgs)
	}

	startScene(t, sc)
	sc.FireOnStart("system:scene-activated")

	// Both blueprints' on-start fired; each wrote its disjoint var.
	waitForState(t, sc, "__vars.bpA.counter", `7`, time.Second)
	waitForState(t, sc, "__vars.bpB.counter", `9`, time.Second)
}

// TestExecMulti_VarsDisjointBetweenBlueprints (criterion #4, B9): the
// two blueprints of ONE scene have fully disjoint `__vars` — bpA's
// write never lands under bpB's namespace and vice versa.
func TestExecMulti_VarsDisjointBetweenBlueprints(t *testing.T) {
	sc := multiScene(t, "disjoint", setProg("bpA", 11), setProg("bpB", 22))
	startScene(t, sc)
	sc.FireOnStart("system:scene-activated")

	waitForState(t, sc, "__vars.bpA.counter", `11`, time.Second)
	waitForState(t, sc, "__vars.bpB.counter", `22`, time.Second)

	// Cross-namespace bleed check: neither var carries the other's value.
	if v, _ := sc.state.Get("__vars.bpA.counter"); string(v) != `11` {
		t.Fatalf("bpA var = %s, want 11 (no bleed from bpB)", v)
	}
	if v, _ := sc.state.Get("__vars.bpB.counter"); string(v) != `22` {
		t.Fatalf("bpB var = %s, want 22 (no bleed from bpA)", v)
	}
}

// TestExecMulti_VarsDisjointBetweenInstancesSharingKey (criterion #4,
// B9 of #82, not broken): two instances (live + test session) that share
// a blueprint_key keep disjoint `__vars` even though they SHARE the very
// same immutable program pointer — the path is identical, the State
// object is not.
func TestExecMulti_VarsDisjointBetweenInstancesSharingKey(t *testing.T) {
	shared := setProg("bpA", 42) // one immutable program, two instances

	live := multiScene(t, "share-live", shared)
	session := multiScene(t, "share-session", shared)
	if live.execProgs["bpA"] != session.execProgs["bpA"] {
		t.Fatalf("instances must SHARE the immutable program pointer")
	}
	startScene(t, live)
	startScene(t, session)

	_, before := session.state.Snapshot()

	live.FireOnStart("system:scene-activated")
	waitForState(t, live, "__vars.bpA.counter", `42`, time.Second)

	_, after := session.state.Snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("session state changed after live's write — cross-instance __vars bleed: %v → %v", before, after)
	}
	if v, _ := session.state.Get("__vars.bpA.counter"); string(v) != `0` {
		t.Fatalf("session bpA var = %s, want untouched default 0", v)
	}
}

// TestExecMulti_OnEventRoutesByTopicAcrossBlueprints (criterion #4): the
// merged on-event index routes a `__events.<topic>` write to every
// blueprint that listens to that topic — and ONLY those. bpA listens to
// "chat", bpB to "follow"; each fires independently under its namespaced
// key.
func TestExecMulti_OnEventRoutesByTopicAcrossBlueprints(t *testing.T) {
	mkEventProg := func(key, topic string, value int) *ExecProgram {
		n := varSet("set", "counter", nil, nil)
		n.Config["value"] = raw(strconv.Itoa(value))
		return &ExecProgram{
			BlueprintKey: key,
			Nodes:        map[string]*ExecNode{"set": n},
			Entrypoints: map[string]ExecEntry{
				"on" + topic: {Kind: EntryOnEvent, Event: topic, Target: ExecTarget{Node: "set"}},
			},
		}
	}
	progA := mkEventProg("bpA", "chat", 1)
	progB := mkEventProg("bpB", "follow", 2)

	sc := multiScene(t, "events", progA, progB)

	// Merged on-event index: each topic maps to its namespaced key.
	if got := sc.execOnEvent["chat"]; !reflect.DeepEqual(got, []string{"bpA/onchat"}) {
		t.Fatalf(`on-event["chat"] = %v, want ["bpA/onchat"]`, got)
	}
	if got := sc.execOnEvent["follow"]; !reflect.DeepEqual(got, []string{"bpB/onfollow"}) {
		t.Fatalf(`on-event["follow"] = %v, want ["bpB/onfollow"]`, got)
	}

	startScene(t, sc)

	// A "chat" event fires bpA only.
	sc.Input(InputMsg{Path: eventsPrefix + "chat", Value: raw(`{"user":"x"}`)})
	waitForState(t, sc, "__vars.bpA.counter", `1`, time.Second)
	if v, _ := sc.state.Get("__vars.bpB.counter"); string(v) != `0` {
		t.Fatalf("bpB fired on a chat event it does not listen to: %s", v)
	}

	// A "follow" event fires bpB only.
	sc.Input(InputMsg{Path: eventsPrefix + "follow", Value: raw(`{"user":"y"}`)})
	waitForState(t, sc, "__vars.bpB.counter", `2`, time.Second)
}

// TestExecMulti_IndexMergeDeterministic (criterion #4): InstallExec is a
// pure function of the program set — the merged trigger indexes are
// byte-identical regardless of the slice order the programs arrive in,
// and never derive from map iteration.
func TestExecMulti_IndexMergeDeterministic(t *testing.T) {
	build := func(order int) *Scene {
		a := setProg("bpA", 1)
		b := setProg("bpB", 2)
		c := setProg("bpC", 3)
		sc := NewScene("det", multiVarsGraph("det"), &compiler.RenderBundle{SceneVersion: "sha256:multi-exec-test"}, NewComputeRegistry(), quietLogger())
		switch order {
		case 0:
			sc.InstallExec(a, b, c)
		case 1:
			sc.InstallExec(c, b, a)
		default:
			sc.InstallExec(b, a, c)
		}
		return sc
	}
	want := []string{"bpA/start", "bpB/start", "bpC/start"}
	for ord := 0; ord < 3; ord++ {
		sc := build(ord)
		if !reflect.DeepEqual(sc.execOnStart, want) {
			t.Fatalf("install order %d: on-start index = %v, want %v (order-independent, sorted)", ord, sc.execOnStart, want)
		}
	}
}
