//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Milestone 2 — the live DATA scene (the "+effets data" criterion). The
// first scene that is BOTH reactive (a real Twitch chat event repaints
// live — the M1 guard) AND data-bearing (an exec db.query against
// ZabRanking returns real rows that land on the antenna). It flies the
// EXACT bp-live-data blueprint Blue's fixture pins
// (Blue/tests/fixtures/m2_live_data.py, kept honest by
// Blue/tests/test_m2_live_data.py) and the Canvas layout ZabCanvas pins
// (ZabCanvas/tests/test_m2_live_data_layout.py), through the REAL
// push → validate → activate API against live PG and one shared Show.
//
// The data effect (db.query) is EXEC-BEARING, so the scene goes through
// the #87 validation gate exactly like the canary: an unvalidated scene
// can NEVER fire the query (R9 — asserted by
// TestE2E_LiveData_UnvalidatedSceneNeverQueries below). On air, the
// validated scene's on-start fires db.query → topology-A `_query` through
// the stub gateway → the rows resume onto __vars..ranking_rows.
//
// The db.query DESCRIPTOR is the top-5 leaderboard off ZabRanking's
// player_scores (player_id + score, score desc, limit 5) — the exact
// QueryDescriptor the Blue fixture pins and the value Orion POSTs to
// /ranking/api/v1/_query.

// rankingDatasource / rankingRowsLeaf / liveChatDisplayLeaf are the M2
// cross-repo contract anchors (mirror the Blue fixture + Canvas layout).
const (
	rankingDatasource   = "ranking"
	rankingRowsLeaf     = "__vars..ranking_rows" // legacy single-blueprint key "" → __vars..<var>
	liveChatDisplayLeaf = "chat.display"
)

// liveDataBlueprint is the M2 scene's logic — byte-for-byte the Blue
// fixture's bp-live-data graph (Orion partitions on these ports). Two
// tranches:
//
//	EXEC (data core):
//	  on-start → db.query(datasource=ranking, descriptor=<top-5>)
//	               then → variable.set(ranking_rows = q.rows)
//
//	DATAFLOW (M1 reactive guard):
//	  quasar.twitch.chat(channel) ── payload ─┬─ get-field("actor.display_name") ─┐
//	                                          └─ get-field("payload.text") ───────┤
//	      concat(author, ": ") ──► concat(authorSep, text) ──► output("chat.display")
func liveDataBlueprint() *compiler.BlueprintGraph {
	return &compiler.BlueprintGraph{
		ID: "bp-live-data",
		Nodes: []compiler.BlueprintNode{
			// ===== EXEC tranche =====
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{ePort("then")}},
			{ID: "query", Compute: "core.db.query@1",
				Config: map[string]json.RawMessage{
					"datasource": json.RawMessage(`"` + rankingDatasource + `"`),
					"descriptor": json.RawMessage(`{"table":"player_scores","select":["player_id","score"],"order":[{"column":"score","direction":"desc"}],"limit":5}`),
				},
				Inputs: []compiler.BlueprintPort{ePort("exec_in")},
				Outputs: []compiler.BlueprintPort{
					ePort("then"), ePort("error"),
					dPort("rows"), dPort("count")}},
			{ID: "setRows", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"ranking_rows"`)},
				Inputs:  []compiler.BlueprintPort{ePort("exec_in"), dPort("value")},
				Outputs: []compiler.BlueprintPort{ePort("then")}},

			// ===== DATAFLOW tranche (M1 reactive chat) =====
			{ID: "chat", Compute: "quasar.twitch.chat@1",
				Config: map[string]json.RawMessage{"channel": json.RawMessage(`"` + chatChannel + `"`)}},
			{ID: "author", Compute: "core.data.get-field@1",
				Config: map[string]json.RawMessage{"path": json.RawMessage(`"actor.display_name"`)},
				Inputs: []compiler.BlueprintPort{dPort("record")}},
			{ID: "text", Compute: "core.data.get-field@1",
				Config: map[string]json.RawMessage{"path": json.RawMessage(`"payload.text"`)},
				Inputs: []compiler.BlueprintPort{dPort("record")}},
			{ID: "sep", Compute: "core.literal@1",
				Config:  map[string]json.RawMessage{"value": json.RawMessage(`": "`)},
				Outputs: []compiler.BlueprintPort{dPort("out")}},
			{ID: "authorSep", Compute: "core.string.concat@1",
				Inputs: []compiler.BlueprintPort{dPort("a"), dPort("b")}},
			{ID: "line", Compute: "core.string.concat@1",
				Inputs: []compiler.BlueprintPort{dPort("a"), dPort("b")}},
			{ID: "out", Compute: "core.output@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"` + liveChatDisplayLeaf + `"`)},
				Inputs: []compiler.BlueprintPort{dPort("value")}},
		},
		Edges: []compiler.BlueprintEdge{
			// exec tranche
			{FromNode: "start", FromPort: "then", ToNode: "query", ToPort: "exec_in"},
			{FromNode: "query", FromPort: "then", ToNode: "setRows", ToPort: "exec_in"},
			{FromNode: "query", FromPort: "rows", ToNode: "setRows", ToPort: "value"},
			// dataflow tranche (M1)
			{FromNode: "chat", FromPort: "payload", ToNode: "author", ToPort: "record"},
			{FromNode: "chat", FromPort: "payload", ToNode: "text", ToPort: "record"},
			{FromNode: "author", FromPort: "value", ToNode: "authorSep", ToPort: "a"},
			{FromNode: "sep", FromPort: "out", ToNode: "authorSep", ToPort: "b"},
			{FromNode: "authorSep", FromPort: "out", ToNode: "line", ToPort: "a"},
			{FromNode: "text", FromPort: "value", ToNode: "line", ToPort: "b"},
			{FromNode: "line", FromPort: "out", ToNode: "out", ToPort: "value"},
		},
	}
}

// liveDataManifest is the COMPLETE op set bp-live-data binds: the exec
// tranche (on-start, db.query, variable.set) + the dataflow tranche (M1).
// db.query is the single egress op — the "+effets data" criterion.
func liveDataManifest() compiler.ComputeManifest {
	return compiler.ComputeManifest{
		"core.event.on-start@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.db.query@1":       {IsPure: false, IsBounded: false, Version: "1"},
		"core.variable.set@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"quasar.twitch.chat@1":  {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.get-field@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.literal@1":        {IsPure: true, IsBounded: true, Version: "1"},
		"core.string.concat@1":  {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":         {IsPure: true, IsBounded: true, Version: "1"},
	}
}

func liveDataFetcher() *stubFetcher {
	return &stubFetcher{
		layouts:    map[string]*compiler.CanvasLayout{"v1": liveDataLayout()},
		blueprints: map[string]*compiler.BlueprintGraph{"bp-live-data": liveDataBlueprint()},
		manifest:   liveDataManifest(),
	}
}

// liveDataLayout renders both reactive surfaces: a ranking text element
// bound to __vars..ranking_rows (the db.query rows) and a chat text
// element bound to chat.display (the M1 line). Mirrors the ZabCanvas
// fixture's served shape.
func liveDataLayout() *compiler.CanvasLayout {
	return &compiler.CanvasLayout{
		Version: "v1",
		Root: compiler.LayoutNode{
			Kind: "stack", ID: "root",
			Children: []compiler.LayoutNode{
				{Kind: "text", ID: "ranking", Bindings: map[string]string{"text": rankingRowsLeaf}},
				{Kind: "text", ID: "chatline", Bindings: map[string]string{"text": liveChatDisplayLeaf}},
			},
		},
	}
}

// liveDataEffects builds the prod-shaped SceneEffects: a real worker pool,
// deny-all egress (db.query needs no egress policy), and a topology-A
// db.query client pointed at the stub gateway with the ranking datasource
// declared. The token func returns a static service token the stub asserts.
func liveDataEffects(t *testing.T, gatewayURL string) *runtime.SceneEffects {
	t.Helper()
	r := effects.NewRunner(4, 64, testGateLogger())
	r.Start()
	t.Cleanup(r.Stop)
	return &runtime.SceneEffects{
		Runner:      r,
		Egress:      effects.NewEgressPolicy(nil, false),
		DB:          effects.NewDBQueryClient(gatewayURL, "orion-svc-token", nil),
		DataSources: map[string]effects.DataSource{rankingDatasource: {Name: rankingDatasource, Svc: rankingDatasource}},
	}
}

// rankingQueryRows is the real-shaped `_query` 200 response the stub
// gateway returns: five player grades, highest first — what ZabRanking's
// /_query emits for the top-5 player_scores descriptor (UUIDs serialised
// as strings, Numeric score serialised as float, per internal.py::_serialise).
const rankingQueryRows = `{"rows":[` +
	`{"player_id":"11111111-1111-1111-1111-111111111111","score":9.5},` +
	`{"player_id":"22222222-2222-2222-2222-222222222222","score":8.0},` +
	`{"player_id":"33333333-3333-3333-3333-333333333333","score":7.5},` +
	`{"player_id":"44444444-4444-4444-4444-444444444444","score":6.0},` +
	`{"player_id":"55555555-5555-5555-5555-555555555555","score":4.5}` +
	`],"count":5,"elapsed_ms":3}`

// TestE2E_LiveData_FirstFlight flies the M2 live-data scene end to end:
// push (exec-bearing) → activate-before-validate refused → validate →
// activate → on-start fires db.query against the stub `_query` and the
// real rows land on __vars..ranking_rows; AND a real-shaped chat event
// repaints chat.display live (the M1 reactive guard). This is the
// "+effets data" franchissement: reactive AND a real db.query to air.
func TestE2E_LiveData_FirstFlight(t *testing.T) {
	st := requireDB(t)

	// Stub ZabRanking `_query` behind a fake ZabGate: assert the wire path,
	// the descriptor, and the service-token bearer; return the top-5 rows.
	var gotPath, gotAuth atomic.Value
	var gotBody atomic.Value
	gotPath.Store("")
	gotAuth.Store("")
	gotBody.Store("")
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotAuth.Store(r.Header.Get("Authorization"))
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotBody.Store(string(body))
		_, _ = w.Write([]byte(rankingQueryRows))
	}))
	defer gw.Close()

	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "m2-live-data"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, liveDataFetcher())
	// Arm the effect seam on the show BEFORE activation: a validated,
	// exec-bearing scene gets the world ops installed at load (R9 wiring).
	show.SetEffects(liveDataEffects(t, gw.URL))
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	// Push the exec-bearing scene — compiles + persists (db.query is
	// ACCEPTED at compile; ADR 006 partition — never an IMPURE_COMPUTE reject).
	if code, body := operatorPost(t, base+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"bp-live-data"}`); code != 200 {
		t.Fatalf("push of live-data scene = %d %v", code, body)
	}

	// The #87 gate holds for the data scene too: no scene airs unvalidated.
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != http.StatusConflict ||
		body["code"] != "SCENE_NOT_VALIDATED" {
		t.Fatalf("activate-before-validate = %d %v, want 409 SCENE_NOT_VALIDATED", code, body)
	}

	// Validate (db.query resolves to its synthetic {rows:[],count:0} in
	// validation mode — never touches the gateway), then activate.
	if code, _ := operatorPost(t, base+"/validate", `{}`); code != http.StatusAccepted {
		t.Fatalf("validate = %d", code)
	}
	waitValidated(t, base)
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate-after-validate = %d %v", code, body)
	}

	// Proof 1 (the data criterion): on-start fired db.query → topology-A
	// `_query` → the real rows resumed onto __vars..ranking_rows.
	assertLiveDataLeaf(t, show, rankingRowsLeaf, func(v string) bool {
		return strings.Contains(v, "11111111-1111-1111-1111-111111111111") &&
			strings.Contains(v, "9.5") && strings.Contains(v, "55555555")
	})

	// The query hit the correct wire path with the descriptor + service token.
	if p := gotPath.Load().(string); p != "/ranking/api/v1/_query" {
		t.Fatalf("db.query hit wrong wire path: %q, want /ranking/api/v1/_query", p)
	}
	if a := gotAuth.Load().(string); a != "Bearer orion-svc-token" {
		t.Fatalf("db.query missing service-token bearer: %q", a)
	}
	if b := gotBody.Load().(string); !strings.Contains(b, `"table":"player_scores"`) ||
		!strings.Contains(b, `"limit":5`) {
		t.Fatalf("db.query posted wrong descriptor: %q", b)
	}

	// Proof 2 (the M1 reactive guard): a real-shaped Twitch chat event
	// repaints chat.display live — the reactive pipeline still carries a
	// real platform event onto the antenna alongside the data effect.
	active := show.Active()
	if active == nil {
		t.Fatal("no active scene after activation")
	}
	event := `{"platform":"twitch","channel":"g2nmathias","type":"chat",` +
		`"ts":"2026-06-11T12:00:00Z",` +
		`"actor":{"platform_user_id":"678","display_name":"clodocapeo",` +
		`"is_subscriber":true,"is_moderator":true,"badges":["broadcaster/1"]},` +
		`"payload":{"text":"on est live","emotes":[],"reply_to":null}}`
	active.Input(runtime.InputMsg{
		Path:   chatLeaf,
		Value:  json.RawMessage(event),
		Source: "quasar:twitch",
	})
	assertLiveDataLeaf(t, show, liveChatDisplayLeaf, func(v string) bool {
		return v == `"clodocapeo: on est live"`
	})
}

// TestE2E_LiveData_UnvalidatedSceneNeverQueries (R9, load-bearing): a
// pushed-but-NOT-validated live-data scene can never reach air, so its
// db.query can never fire. The gate refuses activation (SCENE_NOT_VALIDATED)
// and the stub `_query` gateway is NEVER hit — proving an unvalidated
// exec-bearing scene touches no database. This is the negative half of the
// "+effets data" criterion: the data effect is gated by validation.
func TestE2E_LiveData_UnvalidatedSceneNeverQueries(t *testing.T) {
	st := requireDB(t)

	var gatewayHit atomic.Bool
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gatewayHit.Store(true)
		_, _ = w.Write([]byte(rankingQueryRows))
	}))
	defer gw.Close()

	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "m2-live-data-gated"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, liveDataFetcher())
	show.SetEffects(liveDataEffects(t, gw.URL))
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	// Push, but DO NOT validate.
	if code, _ := operatorPost(t, base+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"bp-live-data"}`); code != 200 {
		t.Fatalf("push = %d", code)
	}

	// Activation is refused — the scene never airs, so on-start never fires.
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != http.StatusConflict ||
		body["code"] != "SCENE_NOT_VALIDATED" {
		t.Fatalf("activate of unvalidated scene = %d %v, want 409 SCENE_NOT_VALIDATED", code, body)
	}

	// Settle: even after the refused activation, no query can have fired.
	time.Sleep(100 * time.Millisecond)
	if gatewayHit.Load() {
		t.Fatal("R9 VIOLATION: an unvalidated live-data scene reached the _query gateway")
	}
	if a := show.Active(); a != nil {
		t.Fatalf("an unvalidated scene became active: %q", a.Graph().SceneVersion)
	}
}

// TestE2E_LiveData_BindsTheDataEffect is the structural guard: the M2
// scene binds EXACTLY one egress op — core.db.query@1 — the "+effets data"
// criterion. The rest of the op set is dataflow / exec-logic. Proven by
// construction so the scene's data-bearing claim cannot silently regress
// to a no-effects scene.
func TestE2E_LiveData_BindsTheDataEffect(t *testing.T) {
	var egress []string
	for op := range liveDataManifest() {
		if strings.Contains(op, "http") || strings.Contains(op, "db") ||
			strings.Contains(op, "source") || strings.Contains(op, "request") {
			egress = append(egress, op)
		}
	}
	if len(egress) != 1 || egress[0] != "core.db.query@1" {
		t.Fatalf("live-data egress op set = %v, want exactly [core.db.query@1]", egress)
	}
}

// assertLiveDataLeaf polls the active scene's snapshot until the given leaf
// satisfies pred (mirrors assertReactiveLeaf / assertCanaryLeaf).
func assertLiveDataLeaf(t *testing.T, show *runtime.Show, key string, pred func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if a := show.Active(); a != nil {
			sub, snap := a.Subscribe(8)
			a.Detach(sub)
			if v, ok := snap.State[key]; ok {
				last = string(v)
				if pred(last) {
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("leaf %s never satisfied predicate on air; last = %q", key, last)
}
