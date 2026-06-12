package runtime

import (
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Stream-rule routing + lifecycle tests (ADR 009 §3.3/§3.4, issue #153).
//
// A promoted stream rule is a roster scene that runs ALWAYS: it is in the
// RouteTargets union alongside the active scene, it is never gated on the
// active pointer, and it is never frozen (CancelExec/SetOnAir(false)) at a
// scene switch because it never enters SetActive. These tests prove the
// union at the Show seam, the always-live lifecycle, and the disjointness
// of the non-promoted roster (criterion #1 ADR 008 stays vert).

// idsOf maps a RouteTargets slice to its scene ids, in order.
func idsOf(scenes []*Scene) []string {
	out := make([]string, 0, len(scenes))
	for _, s := range scenes {
		out = append(out, s.ID())
	}
	return out
}

func eqIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// onTickIncProg fires `on-tick` → variable.set(counter = counter + 1),
// reading the prior tick's value via add.counter (varsGraph). One
// increment per tick — a monotone liveness witness that a gated, frozen
// scene would stall (counter stops climbing) but a live rule keeps
// climbing across a scene switch.
func onTickIncProg() *ExecProgram {
	return &ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*ExecNode{
			"inc": varSet("inc", "counter",
				[]ExecDataInput{{Port: "value", From: "add.counter"}}, nil),
		},
		Entrypoints: map[string]ExecEntry{
			"tick": {Target: ExecTarget{Node: "inc"}, Kind: EntryOnTick, Node: "tickn"},
		},
	}
}

// TestRouteTargets_UnionActivePlusRules: RouteTargets returns
// {active} ∪ {promoted rules}, active first, rules sorted by id, and a
// non-promoted roster scene is absent (disjoint backstage).
func TestRouteTargets_UnionActivePlusRules(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	show.LoadExec("scene-active", varsGraph("scene-active"), bundle)
	show.LoadExec("scene-dormant", varsGraph("scene-dormant"), bundle)
	if err := show.SetActive("scene-active", nil); err != nil {
		t.Fatal(err)
	}

	// No rules yet: only the active scene is routed to.
	if got := idsOf(show.RouteTargets()); !eqIDs(got, []string{"scene-active"}) {
		t.Fatalf("RouteTargets = %v, want [scene-active] (no rules promoted)", got)
	}

	// Promote two rules (out of alphabetical order to prove the sort).
	if err := show.PromoteStreamRule("rule-zeta", varsGraph("rule-zeta"), bundle); err != nil {
		t.Fatalf("PromoteStreamRule zeta: %v", err)
	}
	if err := show.PromoteStreamRule("rule-alpha", varsGraph("rule-alpha"), bundle); err != nil {
		t.Fatalf("PromoteStreamRule alpha: %v", err)
	}

	// Active first, then rules sorted by id. The dormant roster scene is
	// NOT in the union (criterion #1 ADR 008).
	got := idsOf(show.RouteTargets())
	want := []string{"scene-active", "rule-alpha", "rule-zeta"}
	if !eqIDs(got, want) {
		t.Fatalf("RouteTargets = %v, want %v", got, want)
	}
	for _, id := range got {
		if id == "scene-dormant" {
			t.Fatal("non-promoted, non-active roster scene leaked into RouteTargets")
		}
	}

	// Demote a rule: it leaves the union.
	show.DemoteStreamRule("rule-alpha")
	if got := idsOf(show.RouteTargets()); !eqIDs(got, []string{"scene-active", "rule-zeta"}) {
		t.Fatalf("after demote RouteTargets = %v, want [scene-active rule-zeta]", got)
	}
}

// TestRouteTargets_RuleWithoutActiveScene: with no active scene, the union
// is exactly the promoted rules — events between scenes still reach rules
// (ADR 009 §3.3 (b)).
func TestRouteTargets_RuleWithoutActiveScene(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	if err := show.PromoteStreamRule("rule-1", varsGraph("rule-1"), bundle); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}
	// No SetActive call → active == "".
	if got := idsOf(show.RouteTargets()); !eqIDs(got, []string{"rule-1"}) {
		t.Fatalf("RouteTargets with no active = %v, want [rule-1]", got)
	}
}

// TestRouteTargets_DisjointFromActive: a promoted rule can never be the
// active scene, so the union never double-counts and SetActive rejects it.
func TestRouteTargets_DisjointFromActive(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	if err := show.PromoteStreamRule("rule-1", varsGraph("rule-1"), bundle); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}
	if !show.IsStreamRule("rule-1") {
		t.Fatal("IsStreamRule(rule-1) = false after promotion")
	}
	if err := show.SetActive("rule-1", nil); err != ErrRuleIsActiveScene {
		t.Fatalf("SetActive(rule) err = %v, want ErrRuleIsActiveScene", err)
	}

	// And promoting the active scene is refused.
	show.LoadExec("scene-active", varsGraph("scene-active"), bundle)
	if err := show.SetActive("scene-active", nil); err != nil {
		t.Fatal(err)
	}
	if err := show.PromoteStreamRule("scene-active", varsGraph("scene-active"), bundle); err != ErrRuleIsActiveScene {
		t.Fatalf("PromoteStreamRule(active) err = %v, want ErrRuleIsActiveScene", err)
	}
}

// TestStreamRule_FiresOnTickWithoutActiveScene (criterion #2): a promoted
// rule fires its on-tick chain with NO active scene — the rule is ungated
// and on-air, so the tick lands and the effect runs.
func TestStreamRule_FiresOnTickWithoutActiveScene(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	if err := show.PromoteStreamRule("rule-1", varsGraph("rule-1"), bundle, onTickSetProg()); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}
	rule, _ := show.Get("rule-1")

	// Fan the tick exactly as Tick.fanout does — to every RouteTargets.
	for _, s := range show.RouteTargets() {
		tick(t, s, 1000)
	}
	// No active scene, yet the rule fired.
	waitForState(t, rule, "__vars.bp.ticked", "1", time.Second)
}

// TestStreamRule_NotFrozenBySceneSwitch (criterion #2/#3, CRITICAL): a
// promoted rule keeps firing on-tick across an A→B switch. A gated roster
// scene would be frozen off-air on the switch; the rule must not be — it
// never enters SetActive, so CancelExec/SetOnAir(false) never touch it.
func TestStreamRule_NotFrozenBySceneSwitch(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	show.LoadExec("scene-a", varsGraph("scene-a"), bundle)
	show.LoadExec("scene-b", varsGraph("scene-b"), bundle)
	if err := show.PromoteStreamRule("rule-1", varsGraph("rule-1"), bundle, onTickIncProg()); err != nil {
		t.Fatalf("PromoteStreamRule: %v", err)
	}
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	rule, _ := show.Get("rule-1")

	// First tick (active = A): the rule increments to 1.
	for _, s := range show.RouteTargets() {
		tick(t, s, 1000)
	}
	waitForState(t, rule, "__vars.bp.counter", "1", time.Second)

	// Switch A→B: a gated roster scene would be frozen off-air here. The
	// rule never enters SetActive, so CancelExec/SetOnAir(false) never
	// touch it. A post-switch tick must keep the counter climbing.
	if err := show.SetActive("scene-b", nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range show.RouteTargets() {
		tick(t, s, 2000)
	}
	// If the switch had frozen the rule, the counter would stall at 1.
	waitForState(t, rule, "__vars.bp.counter", "2", time.Second)

	// A second switch B→A: still climbing — state survived A→B→A.
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range show.RouteTargets() {
		tick(t, s, 3000)
	}
	waitForState(t, rule, "__vars.bp.counter", "3", time.Second)
}

// TestStreamRule_RosterSceneInertNoRecompute (criterion #1 ADR 008): a
// non-promoted, non-active roster scene is absent from RouteTargets, so a
// tick fanned over the union never reaches it — zero on-tick fire.
func TestStreamRule_RosterSceneInertNoRecompute(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:exec-test"}

	show.LoadExec("scene-active", varsGraph("scene-active"), bundle, onTickSetProg())
	show.LoadExec("scene-dormant", varsGraph("scene-dormant"), bundle, onTickSetProg())
	if err := show.SetActive("scene-active", nil); err != nil {
		t.Fatal(err)
	}
	active, _ := show.Get("scene-active")
	dormant, _ := show.Get("scene-dormant")

	for _, s := range show.RouteTargets() {
		tick(t, s, 1000)
	}
	// Active fired; dormant never received the tick (not in the union).
	waitForState(t, active, "__vars.bp.ticked", "1", time.Second)
	if v, ok := dormant.state.Get("__vars.bp.ticked"); ok && string(v) != "0" {
		t.Fatalf("dormant roster scene fired on-tick (ticked=%s) — leaked into routing", v)
	}
}
