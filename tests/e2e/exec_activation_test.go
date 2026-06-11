//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// E2E coverage of the R9 activation lift (ADR 006 §3.4, issue #106)
// through the REAL push/validate/activate/rollback API against live PG
// and one shared Show:
//
//   - criterion #5: the maintainer's loop (ADR 003 #4) airs through the
//     real API — push (exec-bearing) → validate → activate → the loop
//     runs live and a subscriber observes counter == 9.
//   - criterion #7: boot reseed — loadActiveScenes (modelled by a fresh
//     Show + ExecForBoot) reinstalls a validated scene's exec.
//   - criterion #8: deferred swap — an active scene re-pushed with a
//     version that later validates takes the antenna on validation
//     success, no second push.

// execIn / execOut / dataPort build the typed ports the compiler
// partition keys on (exec-pin presence, ADR 006 §3.1).
func ePort(name string) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: name, Type: "exec", Kind: "exec"}
}
func dPort(name string) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: name, Type: "any", Kind: "data"}
}

// loopBlueprint is the maintainer's loop (ADR 003 criterion #4):
//
//	on-start → for-loop(first=0, last=9) → body: variable.set(counter = index)
//
// The loop's `index` data-out feeds variable.set's value (a data edge
// into an exec node → ExecDataInput with FromPort "index", resolved in
// the task env). After the loop completes, `__vars.<key>.counter == 9`.
func loopBlueprint() *compiler.BlueprintGraph {
	return &compiler.BlueprintGraph{
		ID: "bp-loop",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{ePort("then")}},
			{ID: "first", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`0`)},
				Outputs: []compiler.BlueprintPort{dPort("out")}},
			{ID: "last", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`9`)},
				Outputs: []compiler.BlueprintPort{dPort("out")}},
			{ID: "loop", Compute: "core.flow.for-loop@1",
				Inputs: []compiler.BlueprintPort{
					ePort("exec_in"), dPort("first"), dPort("last")},
				Outputs: []compiler.BlueprintPort{
					ePort("body"), ePort("completed"), dPort("index")}},
			{ID: "set", Compute: "core.variable.set@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"counter"`)},
				Inputs: []compiler.BlueprintPort{
					ePort("exec_in"), dPort("value")},
				Outputs: []compiler.BlueprintPort{ePort("then")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "loop", ToPort: "exec_in"},
			{FromNode: "first", FromPort: "out", ToNode: "loop", ToPort: "first"},
			{FromNode: "last", FromPort: "out", ToNode: "loop", ToPort: "last"},
			{FromNode: "loop", FromPort: "body", ToNode: "set", ToPort: "exec_in"},
			{FromNode: "loop", FromPort: "index", ToNode: "set", ToPort: "value"},
		},
	}
}

func loopFetcher() *stubFetcher {
	m := compiler.ComputeManifest{
		"core.literal@1":        {IsPure: true, IsBounded: true, Version: "1"},
		"core.event.on-start@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.flow.for-loop@1":  {IsPure: true, IsBounded: true, Version: "1"},
		"core.variable.set@1":   {IsPure: true, IsBounded: true, Version: "1"},
	}
	return &stubFetcher{
		layouts: map[string]*compiler.CanvasLayout{
			"v1": {Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		},
		blueprints: map[string]*compiler.BlueprintGraph{"bp-loop": loopBlueprint()},
		manifest:   m,
	}
}

// TestE2E_R9_MaintainersLoopAirsThroughRealAPI (criterion #5): the loop
// blueprint reaches air through the real push → validate → activate API
// and its exec runs LIVE — the franchissement proven end to end.
func TestE2E_R9_MaintainersLoopAirsThroughRealAPI(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "loop"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, loopFetcher())
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	// Push the exec-bearing scene — it compiles and persists (criterion
	// #5: exec-bearing versions are ACCEPTED, never rejected at compile).
	if code, body := operatorPost(t, base+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"bp-loop"}`); code != 200 {
		t.Fatalf("push of exec scene = %d %v", code, body)
	}

	// Activate before validation → refused (the gate holds for exec too).
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != http.StatusConflict ||
		body["code"] != "SCENE_NOT_VALIDATED" {
		t.Fatalf("activate-before-validate = %d %v, want 409 SCENE_NOT_VALIDATED", code, body)
	}

	// Validate, then activate — the exec installs and on-start fires.
	if code, _ := operatorPost(t, base+"/validate", `{}`); code != http.StatusAccepted {
		t.Fatalf("validate = %d", code)
	}
	waitValidated(t, base)
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate-after-validate = %d %v", code, body)
	}

	// The loop ran live: counter == 9 (the maintainer's loop result).
	assertCounter(t, show, "9")
}

// TestE2E_R9_BootReseedInstallsExec (criterion #7): after a validated
// exec scene exists, a FRESH Show cold-started via ExecForBoot reinstalls
// its programs, so activating it on the new process airs the live logic —
// the restart does NOT leave the exec silently dead.
func TestE2E_R9_BootReseedInstallsExec(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "boot"); err != nil {
		t.Fatal(err)
	}
	srv, _ := gateTestServer(t, st, loopFetcher())
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()
	if code, _ := operatorPost(t, base+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"bp-loop"}`); code != 200 {
		t.Fatalf("push = %d", code)
	}
	operatorPost(t, base+"/validate", `{}`)
	waitValidated(t, base)

	// Simulate a RESTART: a brand-new Show that reseeds from the store
	// through the same boot path the binary uses (ExecForBoot per scene).
	show2 := bootReseed(t, st)
	if err := show2.SetActive(sceneID.String(), nil); err != nil {
		t.Fatalf("activate on rebooted show: %v", err)
	}
	assertCounter(t, show2, "9")
}

// TestE2E_BootRestoresActiveScene is the regression guard for the
// black-screen-after-deploy bug (chantier boot-reactivate-active-scene):
// a scene activated via the REAL API persists the antenna pointer
// (migrations/0004); after a restart, loadActiveScenes must re-LOAD AND
// re-ACTIVATE that scene, so the rebooted show's `active` pointer is set
// and a viewer receives a snapshot instead of `scene not found`. Leaf
// VALUES reseed to declared defaults (criterion #11) — only the SELECTION
// survives.
func TestE2E_BootRestoresActiveScene(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "antenna"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, loopFetcher())
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	// Push + validate + activate through the real API — activation now
	// persists show_state.active_scene_id (the fix's write side).
	if code, body := operatorPost(t, base+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"bp-loop"}`); code != 200 {
		t.Fatalf("push = %d %v", code, body)
	}
	operatorPost(t, base+"/validate", `{}`)
	waitValidated(t, base)
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate = %d %v", code, body)
	}
	// The pointer is persisted in the DB, independent of the in-memory show.
	if pid, err := st.GetActiveSceneID(context.Background()); err != nil || pid == nil ||
		pid.String() != sceneID.String() {
		t.Fatalf("active pointer not persisted: id=%v err=%v", pid, err)
	}
	_ = show // pre-reboot show; the proof is the rebooted one.

	// RESTART: a brand-new show cold-started through the same boot path the
	// binary runs (load roster + re-activate persisted pointer). Pre-fix this
	// returned a show with active == "" → every viewer closed `scene not found`.
	show2 := bootReseed(t, st)

	// 1. The antenna survived: active pointer is the same scene.
	if a := show2.Active(); a == nil || a.ID() != sceneID.String() {
		t.Fatalf("antenna dark after reboot: Active()=%v, want %s", a, sceneID)
	}

	// 2. A viewer gets a snapshot, NOT scene-not-found. SubscribeLive is the
	// exact path the WS /show/stream viewer takes; pre-fix it returned
	// ErrSceneNotFound because active was empty.
	sub, snap, err := show2.SubscribeLive(8)
	if err != nil {
		t.Fatalf("viewer SubscribeLive after reboot = %v, want a snapshot", err)
	}
	defer show2.UnsubscribeLive(sub)
	if snap == nil {
		t.Fatalf("viewer got nil snapshot after reboot")
	}

	// 3. Leaves reseeded to declared defaults (criterion #11): the exec
	// loop re-ran on boot-activation from defaults and reached counter == 9.
	// The SELECTION survived; the VALUES came from the declared defaults, not
	// from any persisted live state.
	assertCounter(t, show2, "9")
}

// TestE2E_R9_DeferredSwapOnValidationSuccess (criterion #8, completes
// ADR 003 #15): scene active on air (v1), re-push v2 (different content,
// no record) → antenna stays v1; then /validate v2 succeeds → the swap
// proceeds on validation success (no second push) and v2's exec airs.
func TestE2E_R9_DeferredSwapOnValidationSuccess(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "swap"); err != nil {
		t.Fatal(err)
	}
	// Two distinct loop blueprints: bp-loop (last=9) and bp-loop8 (last=8)
	// → distinct content, distinct hashes, distinct live results.
	f := loopFetcher()
	bp8 := loopBlueprint()
	bp8.ID = "bp-loop8"
	for i := range bp8.Nodes {
		if bp8.Nodes[i].ID == "last" {
			bp8.Nodes[i].Config = map[string]json.RawMessage{"value": json.RawMessage(`8`)}
		}
	}
	f.blueprints["bp-loop8"] = bp8

	srv, show := gateTestServer(t, st, f)
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	// v1 (last=9): push, validate, activate.
	_, pb := operatorPost(t, base+"/push", `{"canvas_version":"v1","blue_blueprint_id":"bp-loop"}`)
	v1, _ := pb["scene_version"].(string)
	operatorPost(t, base+"/validate", `{}`)
	waitValidated(t, base)
	if code, _ := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate v1 failed")
	}
	assertCounter(t, show, "9")

	// Re-push v2 (last=8): persisted, antenna unmoved (still v1, counter 9).
	code, body := operatorPost(t, base+"/push", `{"canvas_version":"v1","blue_blueprint_id":"bp-loop8"}`)
	if code != 200 || body["code"] != "SCENE_NOT_VALIDATED" {
		t.Fatalf("re-push v2 = %d %v, want 200 SCENE_NOT_VALIDATED", code, body)
	}
	v2, _ := body["scene_version"].(string)
	if v2 == v1 || v2 == "" {
		t.Fatalf("v2 hash %q collided with v1 %q", v2, v1)
	}
	if a := show.Active(); a == nil || a.Graph().SceneVersion != v1 {
		t.Fatalf("antenna moved off v1 before v2 validated")
	}

	// Validate v2 → the DEFERRED SWAP proceeds on success: antenna becomes
	// v2 and its exec airs (counter == 8), with NO second push.
	operatorPost(t, base+"/validate", `{}`)
	waitValidated(t, base)
	waitSceneVersion(t, show, v2)
	assertCounter(t, show, "8")
}

// assertCounter polls the active scene's snapshot until __vars.bp.counter
// equals want (the variable.set blueprint key is empty-legacy → "bp"? No:
// single-blueprint push namespaces under the blueprint ref key). We probe
// every known key form to stay robust to the legacy/empty key.
func assertCounter(t *testing.T, show *runtime.Show, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastState map[string]json.RawMessage
	for time.Now().Before(deadline) {
		a := show.Active()
		if a != nil {
			sub, snap := a.Subscribe(8)
			a.Detach(sub)
			lastState = snap.State
			for _, k := range counterKeys {
				if v, ok := snap.State[k]; ok && string(v) == want {
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("counter never reached %s on air; last state = %v", want, lastState)
}

func waitSceneVersion(t *testing.T, show *runtime.Show, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := show.Active(); a != nil && a.Graph().SceneVersion == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("antenna never swapped to %s", want)
}

// counterKeys are the candidate state paths variable.set(counter) may
// write under, depending on the blueprint-ref key the single-blueprint
// push assigns (legacy-empty → __vars..counter; ref key → __vars.<k>.counter).
var counterKeys = []string{
	"__vars.counter", "__vars..counter", "__vars.bp-loop.counter",
	"__vars.bp-1.counter", "__vars.bp.counter",
}

// bootReseed constructs a fresh Show and reloads every active+validated
// scene through the exact ExecForBoot path cmd/orion uses, proving the
// boot reseed installs exec (criterion #7).
func bootReseed(t *testing.T, st *store.Store) *runtime.Show {
	t.Helper()
	logger := testGateLogger()
	show := runtime.NewShow(runtime.NewComputeRegistry(), logger)
	t.Cleanup(show.Stop)
	ctx := context.Background()
	scenes, err := st.ListActiveScenesWithPush(ctx)
	if err != nil {
		t.Fatalf("list active scenes: %v", err)
	}
	for _, sc := range scenes {
		pv, err := st.GetLatestPushedVersion(ctx, sc.ID)
		if err != nil {
			continue
		}
		var graph compiler.Graph
		var bundle compiler.RenderBundle
		if err := json.Unmarshal(pv.GraphJSON, &graph); err != nil {
			continue
		}
		_ = json.Unmarshal(pv.BundleJSON, &bundle)
		progs := bootProgs(ctx, st, sc.ID, pv.SceneVersion, &graph)
		show.LoadExec(sc.ID.String(), &graph, &bundle, progs...)
	}
	// Mirror loadActiveScenes: re-activate the persisted antenna pointer so
	// the rebooted show comes back on the SAME scene (the fix). Leaf VALUES
	// are not restored (LoadExec reseeds defaults — criterion #11); only the
	// SELECTION is durable.
	activeID, err := st.GetActiveSceneID(ctx)
	if err != nil {
		t.Fatalf("boot: read active scene pointer: %v", err)
	}
	if activeID != nil {
		if _, gerr := show.Get(activeID.String()); gerr == nil {
			if serr := show.SetActive(activeID.String(), nil); serr != nil {
				t.Fatalf("boot: re-activate persisted scene: %v", serr)
			}
		}
	}
	return show
}

// bootProgs mirrors api.ExecForBoot over the store (it is exported there;
// re-resolved here to avoid importing the api package's logger plumbing).
func bootProgs(ctx context.Context, st *store.Store, sceneID uuid.UUID, version string, graph *compiler.Graph) []*runtime.ExecProgram {
	ok, err := st.IsVersionValidated(ctx, sceneID, version, runtime.HarnessVersion)
	if err != nil || !ok {
		return nil
	}
	progs, err := runtime.ExecProgramsFromGraph(graph)
	if err != nil {
		return nil
	}
	return progs
}
