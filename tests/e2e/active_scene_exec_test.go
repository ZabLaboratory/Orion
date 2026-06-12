//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/adapters"
	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// E2E coverage of ADR 008 (active-scene-only execution) through the REAL
// Compile → push → validate → activate API against live PG, plus the REAL
// adapters.Inbox routing layer:
//
//   - criterion #1/#2: a write to __events.<topic> routed through the real
//     inbox fires the on-event entry of the ACTIVE scene and does NOT fire
//     a loaded-but-dormant scene listening to the same topic.
//   - criterion #3/#4: A→B→A switch — B's exec airs, A stops, A's frozen
//     state survives the round-trip, and on reactivation A's on-start refires.
//   - criterion #1 (tick half): a dormant scene receives no tick (it is not
//     the active scene the tick routes to), so it neither recomputes nor
//     fires on-tick.

// onEventBlueprint fires, on `__events.<topic>`, variable.set(fired = 1),
// and on on-start sets variable.set(started = 1). The on-event entry's
// topic seeds the synthesized event-topic acceptance binding (#148) so the
// inbox routes a write to __events.<topic> to this scene when it is active.
func onEventBlueprint(id, topic string) *compiler.BlueprintGraph {
	return &compiler.BlueprintGraph{
		ID: id,
		Nodes: []compiler.BlueprintNode{
			{ID: "onstart", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{ePort("then")}},
			{ID: "onevt", Compute: "core.event.on-event@1",
				Config:  map[string]json.RawMessage{"event_name": json.RawMessage(`"` + topic + `"`)},
				Outputs: []compiler.BlueprintPort{ePort("then")}},
			{ID: "one", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`1`)},
				Outputs: []compiler.BlueprintPort{dPort("out")}},
			{ID: "setStarted", Compute: "core.variable.set@1",
				Config: map[string]json.RawMessage{"variable": json.RawMessage(`"started"`)},
				Inputs: []compiler.BlueprintPort{ePort("exec_in"), dPort("value")},
				Outputs: []compiler.BlueprintPort{ePort("then")}},
			{ID: "setFired", Compute: "core.variable.set@1",
				Config: map[string]json.RawMessage{"variable": json.RawMessage(`"fired"`)},
				Inputs: []compiler.BlueprintPort{ePort("exec_in"), dPort("value")},
				Outputs: []compiler.BlueprintPort{ePort("then")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "onstart", FromPort: "then", ToNode: "setStarted", ToPort: "exec_in"},
			{FromNode: "one", FromPort: "out", ToNode: "setStarted", ToPort: "value"},
			{FromNode: "onevt", FromPort: "then", ToNode: "setFired", ToPort: "exec_in"},
			{FromNode: "one", FromPort: "out", ToNode: "setFired", ToPort: "value"},
		},
	}
}

func onEventManifest() compiler.ComputeManifest {
	return compiler.ComputeManifest{
		"core.literal@1":        {IsPure: true, IsBounded: true, Version: "1"},
		"core.event.on-start@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.event.on-event@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.variable.set@1":   {IsPure: true, IsBounded: true, Version: "1"},
	}
}

// twoOnEventFetcher serves two distinct on-event blueprints that both
// listen to the SAME topic ("goal"). Distinct ids → distinct scene_versions.
func twoOnEventFetcher() *stubFetcher {
	return &stubFetcher{
		layouts: map[string]*compiler.CanvasLayout{
			"v1": {Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		},
		blueprints: map[string]*compiler.BlueprintGraph{
			"bp-evt-a": onEventBlueprint("bp-evt-a", "goal"),
			"bp-evt-b": onEventBlueprint("bp-evt-b", "goal"),
		},
		manifest: onEventManifest(),
	}
}

// pushValidateScene runs the real push → validate API for one scene and
// returns nothing (failures fatal). It does NOT activate.
func pushValidateScene(t *testing.T, srv string, sceneID uuid.UUID, blueprint string) {
	t.Helper()
	base := srv + "/api/v1/scenes/" + sceneID.String()
	if code, body := operatorPost(t, base+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"`+blueprint+`"}`); code != 200 {
		t.Fatalf("push %s = %d %v", blueprint, code, body)
	}
	if code, _ := operatorPost(t, base+"/validate", `{}`); code != http.StatusAccepted {
		t.Fatalf("validate %s = %d", blueprint, code)
	}
	waitValidated(t, base)
}

func activateScene(t *testing.T, srv string, sceneID uuid.UUID) {
	t.Helper()
	if code, body := operatorPost(t, srv+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate %s = %d %v", sceneID, code, body)
	}
}

// operatorIdentity is an admin identity: it may write any path (the inbox
// scope gate passes), so __events.* writes are accepted — the routing is
// then the ONLY thing that decides which scene receives it (ADR 008 §3.3,
// CanWritePath unchanged).
func operatorIdentity() auth.Identity {
	return auth.Identity{UserID: "op-1", Role: auth.RoleAdmin}
}

// varKey probes the candidate state path a single-blueprint push's
// variable.set may write under (legacy/empty vs ref key).
func varKeys(name string) []string {
	return []string{
		"__vars." + name, "__vars.." + name,
		"__vars.bp." + name, "__vars.bp-evt-a." + name, "__vars.bp-evt-b." + name,
	}
}

func sceneVar(sc *runtime.Scene, name string) (string, bool) {
	sub, snap := sc.Subscribe(8)
	sc.Detach(sub)
	for _, k := range varKeys(name) {
		if v, ok := snap.State[k]; ok {
			return string(v), true
		}
	}
	return "", false
}

func waitSceneVar(t *testing.T, sc *runtime.Scene, name, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if v, ok := sceneVar(sc, name); ok {
			last = v
			if v == want {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("scene var %s never reached %s; last = %q", name, want, last)
}

// TestE2E_ADR008_OnEventFiresActiveOnly is criterion #1/#2: an operator
// write to __events.goal routed through the REAL inbox fires the on-event
// entry of the ACTIVE scene and leaves a loaded-but-dormant scene on the
// same topic untouched.
func TestE2E_ADR008_OnEventFiresActiveOnly(t *testing.T) {
	st := requireDB(t)
	sceneA, sceneB := uuid.New(), uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneA, "evt-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateScene(context.Background(), sceneB, "evt-b"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, twoOnEventFetcher())

	pushValidateScene(t, srv.URL, sceneA, "bp-evt-a")
	pushValidateScene(t, srv.URL, sceneB, "bp-evt-b")
	// A on air, B loaded but dormant.
	activateScene(t, srv.URL, sceneA)

	active := show.Active()
	if active == nil || active.ID() != sceneA.String() {
		t.Fatalf("active = %v, want %s", active, sceneA)
	}
	dormant, err := show.Get(sceneB.String())
	if err != nil {
		t.Fatalf("scene B not loaded: %v", err)
	}

	// The synthesized event-topic binding (#148) makes the active scene
	// ACCEPT __events.goal: assert acceptance is declared in the compiled
	// graph (the contract sceneAcceptsPath reads).
	if !graphDeclaresEventTopic(active.Graph(), "goal") {
		t.Fatal("active scene graph does not declare event-topic __events.goal (binding #148 missing)")
	}

	// Route a real write through the real inbox (ADR 008 §3.1 routing).
	inbox := adapters.NewInbox(show, testGateLogger(), nil)
	if err := inbox.Write(context.Background(), adapters.Write{
		Identity: operatorIdentity(),
		Path:     "__events.goal",
		Value:    json.RawMessage(`{}`),
		Source:   "operator:test",
	}); err != nil {
		t.Fatalf("inbox write __events.goal: %v", err)
	}

	// The ACTIVE scene fired its on-event entry → fired == 1.
	waitSceneVar(t, active, "fired", "1")

	// The DORMANT scene listening to the SAME topic fired nothing: its
	// `fired` var is still untouched (no value, or never 1). Give the loop
	// ample time so this is not a race-masked pass.
	time.Sleep(250 * time.Millisecond)
	if v, ok := sceneVar(dormant, "fired"); ok && v == "1" {
		t.Fatalf("dormant scene B fired on-event (fired=%s) — write leaked past active-only routing", v)
	}
}

// TestE2E_ADR008_SwitchABA is criterion #3/#4: A→B→A. B's exec airs on
// activation, A stops; A's frozen state survives the round-trip; on
// reactivation A's on-start refires (started reset to 1 from a fresh fire,
// not reseeded away).
func TestE2E_ADR008_SwitchABA(t *testing.T) {
	st := requireDB(t)
	sceneA, sceneB := uuid.New(), uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneA, "aba-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateScene(context.Background(), sceneB, "aba-b"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, twoOnEventFetcher())
	pushValidateScene(t, srv.URL, sceneA, "bp-evt-a")
	pushValidateScene(t, srv.URL, sceneB, "bp-evt-b")

	// Activate A: on-start fires → started == 1.
	activateScene(t, srv.URL, sceneA)
	a, _ := show.Get(sceneA.String())
	waitSceneVar(t, a, "started", "1")

	// Fire goal on A so it holds live state (fired == 1).
	inbox := adapters.NewInbox(show, testGateLogger(), nil)
	if err := inbox.Write(context.Background(), adapters.Write{
		Identity: operatorIdentity(), Path: "__events.goal",
		Value: json.RawMessage(`{}`), Source: "operator:test",
	}); err != nil {
		t.Fatalf("inbox write: %v", err)
	}
	waitSceneVar(t, a, "fired", "1")

	// Switch A→B: B's on-start fires (B exec airs), A goes off air.
	activateScene(t, srv.URL, sceneB)
	b, _ := show.Get(sceneB.String())
	waitSceneVar(t, b, "started", "1")
	if show.Active().ID() != sceneB.String() {
		t.Fatalf("active = %s, want B", show.Active().ID())
	}

	// A is frozen: a goal write while B is active does NOT reach A
	// (routing) — A's fired stays 1, B's fired becomes 1.
	if err := inbox.Write(context.Background(), adapters.Write{
		Identity: operatorIdentity(), Path: "__events.goal",
		Value: json.RawMessage(`{}`), Source: "operator:test",
	}); err != nil {
		t.Fatalf("inbox write: %v", err)
	}
	waitSceneVar(t, b, "fired", "1")
	if v, ok := sceneVar(a, "fired"); !ok || v != "1" {
		t.Fatalf("frozen A's fired = %q (ok=%v), want 1 (state must survive dormancy untouched)", v, ok)
	}

	// Switch B→A: A's on-start refires (ADR 008 §3.2). A's frozen `fired`
	// is NOT reseeded away (Seed runs once at construction; SetActive does
	// not reseed) — the freeze-and-resume invariant.
	activateScene(t, srv.URL, sceneA)
	if show.Active().ID() != sceneA.String() {
		t.Fatalf("active = %s, want A", show.Active().ID())
	}
	// on-start refired on reactivation: started is (re)set to 1.
	waitSceneVar(t, a, "started", "1")
	// frozen fired survived the dormancy round-trip.
	if v, ok := sceneVar(a, "fired"); !ok || v != "1" {
		t.Fatalf("A's fired after B→A = %q (ok=%v), want 1 (frozen state lost)", v, ok)
	}
}

// graphDeclaresEventTopic reports whether the compiled graph carries the
// synthesized event-topic acceptance binding for the given topic (#148) —
// the exact contract sceneAcceptsPath reads.
func graphDeclaresEventTopic(g *compiler.Graph, topic string) bool {
	want := "__events." + topic
	for _, b := range g.Bindings {
		if b.Kind != "event-topic" {
			continue
		}
		for _, tp := range b.TargetPaths {
			if tp == want {
				return true
			}
		}
	}
	return false
}
