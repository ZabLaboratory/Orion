//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Milestone 3 — the NAMED leaderboard scene. M2 put a top-5
// player_scores ranking on the antenna as raw player_id UUIDs + scores.
// M3 resolves each player_id to a human pseudo by querying ZabTruth's
// players table — via the STATIC UNROLL: the top-5 is a fixed shape, so
// the graph spells out five explicit, SEQUENTIAL sub-graphs (one per rank
// k ∈ {0..4}), each firing its own db.query(truth) for ranking_rows[k].
// No dynamic join (the engine has no string-equal / runtime-path /
// in-loop query), no loop (the for-loop forks the body → out-of-order
// queries). The six db.query effects (1 ranking + 5 truth) chain strictly
// in order — each parks/resumes before the next (the dependent-effect
// chain proven by TestEffects_HTTPRequestResumesContinuation).
//
// This file flies bp-live-data-named end to end through the REAL
// push → validate → activate API against live PG + one Show, with a stub
// ZabGate routing /ranking/_query (5 rows) and /truth/_query (the player
// for the id carried in each query's where clause). It asserts:
//
//   - the five truth queries fire SEQUENTIALLY, each carrying the correct
//     id_k in its where clause (positional mapping ranking_rows[k]);
//   - the final leaderboard_display string maps every rank to its REAL
//     pseudo: "1. <pseudo> — <score>\n2. …";
//   - the M1 reactive chat still repaints chat.display live;
//   - R9: an unvalidated scene fires NO query (neither ranking nor truth).
//
// The blueprint here is the Go twin of Blue's pinned fixture
// (Blue/tests/fixtures/m3_named_leaderboard.py) — same ports, same edges,
// same descriptors — kept honest by Blue/tests/test_m3_named_leaderboard.py
// and the Canvas layout ZabCanvas pins.

const (
	truthDatasource        = "truth"
	leaderboardDisplayLeaf = "__vars..leaderboard_display"
	namedRankingRowsVar    = "ranking_rows"
	namedTopN              = 5
)

// playerNames maps each ranking player_id (the M2 top-5 UUIDs) to the
// summoner_name ZabTruth's /truth/_query returns for that id — the LoL
// in-game name (NOT NULL). The static unroll must reproduce THIS mapping
// positionally, rank by rank. These are the REAL top-5 in-game names.
var playerNames = map[string]string{
	"11111111-1111-1111-1111-111111111111": "GIDEON",
	"22222222-2222-2222-2222-222222222222": "Teddy",
	"33333333-3333-3333-3333-333333333333": "Loki",
	"44444444-4444-4444-4444-444444444444": "Gumayusi",
	"55555555-5555-5555-5555-555555555555": "Namgung",
}

// playerIDsByRank is the score-desc order the ranking query returns —
// rank k → player_id. The expected leaderboard reads off this.
var playerIDsByRank = []string{
	"11111111-1111-1111-1111-111111111111",
	"22222222-2222-2222-2222-222222222222",
	"33333333-3333-3333-3333-333333333333",
	"44444444-4444-4444-4444-444444444444",
	"55555555-5555-5555-5555-555555555555",
}

// rankingScores is the score for each rank (parallel to playerIDsByRank).
// Integers so cast.to-string renders them as "9" not "9.0".
var rankingScores = []string{"9", "8", "7", "6", "4"}

// namedRankingRows is the /ranking/_query 200 response: the M2 top-5
// (player_id + score, score desc, limit 5).
func namedRankingRows() string {
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for k := 0; k < namedTopN; k++ {
		if k > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"player_id":%q,"score":%s}`, playerIDsByRank[k], rankingScores[k])
	}
	fmt.Fprintf(&b, `],"count":%d,"elapsed_ms":2}`, namedTopN)
	return b.String()
}

// expectedLeaderboard is the multi-line display the exec spine must
// compose: "1. <pseudo> — <score>\n…" with the REAL mapped pseudos.
func expectedLeaderboard() string {
	lines := make([]string, namedTopN)
	for k := 0; k < namedTopN; k++ {
		lines[k] = fmt.Sprintf("%d. %s — %s", k+1, playerNames[playerIDsByRank[k]], rankingScores[k])
	}
	return strings.Join(lines, "\n")
}

// --- the Go twin of bp-live-data-named --------------------------------

// eNode/dLit/getField are tiny builders mirroring the Blue fixture shape.
func litNode(id string, value json.RawMessage) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.literal@1",
		Config:  map[string]json.RawMessage{"value": value},
		Outputs: []compiler.BlueprintPort{dPort("out")}}
}

func getFieldNode(id, path string) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.data.get-field@1",
		Config:  map[string]json.RawMessage{"path": mustJSON(path)},
		Inputs:  []compiler.BlueprintPort{dPort("record")},
		Outputs: []compiler.BlueprintPort{dPort("value")}}
}

// inputNode reads a state leaf (a __vars..<name> leaf an exec
// variable.set wrote) — core.input resolves it in the demand cone, unlike
// core.variable.get.
func inputNode(id, leaf string) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.input@1",
		Config:  map[string]json.RawMessage{"name": mustJSON(leaf)},
		Outputs: []compiler.BlueprintPort{dPort("out")}}
}

func concatNode(id string) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.string.concat@1",
		Inputs:  []compiler.BlueprintPort{dPort("a"), dPort("b")},
		Outputs: []compiler.BlueprintPort{dPort("out")}}
}

func mustJSON(v string) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// namedLeaderboardBlueprint builds the M3 static-unroll graph.
func namedLeaderboardBlueprint() *compiler.BlueprintGraph {
	g := &compiler.BlueprintGraph{ID: "bp-live-data-named"}
	add := func(n ...compiler.BlueprintNode) { g.Nodes = append(g.Nodes, n...) }
	edge := func(fn, fp, tn, tp string) {
		g.Edges = append(g.Edges, compiler.BlueprintEdge{FromNode: fn, FromPort: fp, ToNode: tn, ToPort: tp})
	}

	// ===== ranking tranche (M2): on-start → query(ranking) → setRows =====
	add(compiler.BlueprintNode{ID: "start", Compute: "core.event.on-start@1",
		Outputs: []compiler.BlueprintPort{ePort("then")}})
	add(compiler.BlueprintNode{ID: "qRank", Compute: "core.db.query@1",
		Config: map[string]json.RawMessage{
			"datasource": mustJSON(rankingDatasource),
			"descriptor": json.RawMessage(`{"table":"player_scores","select":["player_id","score"],"order":[{"column":"score","direction":"desc"}],"limit":5}`),
		},
		Inputs: []compiler.BlueprintPort{ePort("exec_in")},
		Outputs: []compiler.BlueprintPort{
			ePort("then"), ePort("error"), dPort("rows"), dPort("count")}})
	add(compiler.BlueprintNode{ID: "setRows", Compute: "core.variable.set@1",
		Config:  map[string]json.RawMessage{"name": mustJSON(namedRankingRowsVar)},
		Inputs:  []compiler.BlueprintPort{ePort("exec_in"), dPort("value")},
		Outputs: []compiler.BlueprintPort{ePort("then")}})
	edge("start", "then", "qRank", "exec_in")
	edge("qRank", "then", "setRows", "exec_in")
	edge("qRank", "rows", "setRows", "value")

	// ranking_rows read back as a state-leaf input (core.input resolves in
	// the demand cone; an exec data-out pin loses its from_port there).
	add(inputNode("inRows", "__vars..ranking_rows"))

	// ===== five unrolled rank sub-graphs =====
	for k := 0; k < namedTopN; k++ {
		s := func(base string) string { return fmt.Sprintf("%s%d", base, k) }
		// read ranking_rows[k].player_id and .score (STATIC config paths)
		add(getFieldNode(s("getId"), fmt.Sprintf("%d.player_id", k)))
		add(getFieldNode(s("getScore"), fmt.Sprintf("%d.score", k)))
		edge("inRows", "out", s("getId"), "record")
		edge("inRows", "out", s("getScore"), "record")

		// build WhereClause {column:id, op:=, value:id_k}
		add(litNode(s("clauseLit"), json.RawMessage(`{"column":"id","op":"="}`)))
		add(compiler.BlueprintNode{ID: s("clause"), Compute: "core.data.set-field@1",
			Config:  map[string]json.RawMessage{"path": mustJSON("value")},
			Inputs:  []compiler.BlueprintPort{dPort("record"), dPort("value")},
			Outputs: []compiler.BlueprintPort{dPort("out")}})
		edge(s("clauseLit"), "out", s("clause"), "record")
		edge(s("getId"), "value", s("clause"), "value")

		// wrap into where list [clause_k]
		add(litNode(s("whereEmpty"), json.RawMessage(`[]`)))
		add(compiler.BlueprintNode{ID: s("where"), Compute: "core.data.list-append@1",
			Inputs:  []compiler.BlueprintPort{dPort("list"), dPort("element")},
			Outputs: []compiler.BlueprintPort{dPort("out")}})
		edge(s("whereEmpty"), "out", s("where"), "list")
		edge(s("clause"), "out", s("where"), "element")

		// splice where into players descriptor base
		add(litNode(s("descLit"), json.RawMessage(`{"table":"players","select":["display_name","summoner_name"]}`)))
		add(compiler.BlueprintNode{ID: s("desc"), Compute: "core.data.set-field@1",
			Config:  map[string]json.RawMessage{"path": mustJSON("where")},
			Inputs:  []compiler.BlueprintPort{dPort("record"), dPort("value")},
			Outputs: []compiler.BlueprintPort{dPort("out")}})
		edge(s("descLit"), "out", s("desc"), "record")
		edge(s("where"), "out", s("desc"), "value")

		// the k-th sequential db.query(truth, desc_k)  EXEC
		add(compiler.BlueprintNode{ID: s("query"), Compute: "core.db.query@1",
			Config: map[string]json.RawMessage{"datasource": mustJSON(truthDatasource)},
			Inputs: []compiler.BlueprintPort{ePort("exec_in"), dPort("descriptor")},
			Outputs: []compiler.BlueprintPort{
				ePort("then"), ePort("error"), dPort("rows"), dPort("count")}})
		edge(s("desc"), "out", s("query"), "descriptor")

		// land the returned row on a state leaf; read the pseudo back off it.
		add(compiler.BlueprintNode{ID: s("setRow"), Compute: "core.variable.set@1",
			Config:  map[string]json.RawMessage{"name": mustJSON(fmt.Sprintf("row_%d", k))},
			Inputs:  []compiler.BlueprintPort{ePort("exec_in"), dPort("value")},
			Outputs: []compiler.BlueprintPort{ePort("then")}})
		edge(s("query"), "rows", s("setRow"), "value")
		add(inputNode(s("inRow"), fmt.Sprintf("__vars..row_%d", k)))
		// the pseudo reads summoner_name (NOT NULL; the LoL in-game name) —
		// display_name is nullable and the real top-5 carry a NULL one.
		add(getFieldNode(s("name"), "0.summoner_name"))
		edge(s("inRow"), "out", s("name"), "record")

		// compose "<k+1>. <pseudo> — <score>"
		add(litNode(s("rankLit"), mustJSON(fmt.Sprintf("%d. ", k+1))))
		add(litNode(s("dashLit"), mustJSON(" — ")))
		add(compiler.BlueprintNode{ID: s("scoreStr"), Compute: "core.cast.to-string@1",
			Inputs:  []compiler.BlueprintPort{dPort("value")},
			Outputs: []compiler.BlueprintPort{dPort("out")}})
		edge(s("getScore"), "value", s("scoreStr"), "value")
		add(concatNode(s("catA")))
		add(concatNode(s("catB")))
		add(concatNode(s("line")))
		edge(s("rankLit"), "out", s("catA"), "a")
		edge(s("name"), "value", s("catA"), "b")
		edge(s("catA"), "out", s("catB"), "a")
		edge(s("dashLit"), "out", s("catB"), "b")
		edge(s("catB"), "out", s("line"), "a")
		edge(s("scoreStr"), "out", s("line"), "b")
	}

	// ===== join line_0..line_4 with "\n" (pure fold, read directly) =====
	add(litNode("nl", mustJSON("\n")))
	prev := "line0"
	for k := 1; k < namedTopN; k++ {
		sepCat := fmt.Sprintf("joinSep%d", k)
		lineCat := fmt.Sprintf("joinLine%d", k)
		add(concatNode(sepCat))
		add(concatNode(lineCat))
		edge(prev, "out", sepCat, "a")
		edge("nl", "out", sepCat, "b")
		edge(sepCat, "out", lineCat, "a")
		edge(fmt.Sprintf("line%d", k), "out", lineCat, "b")
		prev = lineCat
	}

	// final exec node: variable.set(leaderboard_display = folded join)
	add(compiler.BlueprintNode{ID: "setBoard", Compute: "core.variable.set@1",
		Config:  map[string]json.RawMessage{"name": mustJSON("leaderboard_display")},
		Inputs:  []compiler.BlueprintPort{ePort("exec_in"), dPort("value")},
		Outputs: []compiler.BlueprintPort{ePort("then")}})
	edge(prev, "out", "setBoard", "value")

	// exec spine: setRows → query0 → setRow0 → query1 → … → setRow4 → setBoard
	edge("setRows", "then", "query0", "exec_in")
	for k := 0; k < namedTopN; k++ {
		edge(fmt.Sprintf("query%d", k), "then", fmt.Sprintf("setRow%d", k), "exec_in")
		if k+1 < namedTopN {
			edge(fmt.Sprintf("setRow%d", k), "then", fmt.Sprintf("query%d", k+1), "exec_in")
		} else {
			edge(fmt.Sprintf("setRow%d", k), "then", "setBoard", "exec_in")
		}
	}

	// ===== M1 reactive chat tranche (the reactive guard) =====
	add(compiler.BlueprintNode{ID: "chat", Compute: "quasar.twitch.chat@1",
		Config: map[string]json.RawMessage{"channel": mustJSON(chatChannel)}})
	add(getFieldNode("author", "actor.display_name"))
	add(getFieldNode("text", "payload.text"))
	add(litNode("sep", mustJSON(": ")))
	add(concatNode("authorSep"))
	add(concatNode("chatLine"))
	add(compiler.BlueprintNode{ID: "chatOut", Compute: "core.output@1",
		Config: map[string]json.RawMessage{"name": mustJSON(liveChatDisplayLeaf)},
		Inputs: []compiler.BlueprintPort{dPort("value")}})
	edge("chat", "payload", "author", "record")
	edge("chat", "payload", "text", "record")
	edge("author", "value", "authorSep", "a")
	edge("sep", "out", "authorSep", "b")
	edge("authorSep", "out", "chatLine", "a")
	edge("text", "value", "chatLine", "b")
	edge("chatLine", "out", "chatOut", "value")

	return g
}

func namedLeaderboardManifest() compiler.ComputeManifest {
	return compiler.ComputeManifest{
		"core.event.on-start@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.db.query@1":         {IsPure: false, IsBounded: false, Version: "1"},
		"core.variable.set@1":     {IsPure: true, IsBounded: true, Version: "1"},
		"core.input@1":            {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.get-field@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.set-field@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.list-append@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.literal@1":          {IsPure: true, IsBounded: true, Version: "1"},
		"core.cast.to-string@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.string.concat@1":    {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":           {IsPure: true, IsBounded: true, Version: "1"},
		"quasar.twitch.chat@1":    {IsPure: true, IsBounded: true, Version: "1"},
	}
}

// namedLeaderboardLayout binds a leaderboard text element to the exec
// variable leaf and a chat text element to chat.display.
func namedLeaderboardLayout() *compiler.CanvasLayout {
	return &compiler.CanvasLayout{
		Version: "v1",
		Root: compiler.LayoutNode{
			Kind: "stack", ID: "root",
			Children: []compiler.LayoutNode{
				{Kind: "text", ID: "leaderboard", Bindings: map[string]string{"text": leaderboardDisplayLeaf}},
				{Kind: "text", ID: "chatline", Bindings: map[string]string{"text": liveChatDisplayLeaf}},
			},
		},
	}
}

func namedLeaderboardFetcher() *stubFetcher {
	return &stubFetcher{
		layouts:    map[string]*compiler.CanvasLayout{"v1": namedLeaderboardLayout()},
		blueprints: map[string]*compiler.BlueprintGraph{"bp-live-data-named": namedLeaderboardBlueprint()},
		manifest:   namedLeaderboardManifest(),
	}
}

// namedLeaderboardEffects wires both datasources (ranking + truth) at the
// stub gateway.
func namedLeaderboardEffects(t *testing.T, gatewayURL string) *runtime.SceneEffects {
	t.Helper()
	r := effects.NewRunner(4, 64, testGateLogger())
	r.Start()
	t.Cleanup(r.Stop)
	return &runtime.SceneEffects{
		Runner: r,
		Egress: effects.NewEgressPolicy(nil, false),
		DB:     effects.NewDBQueryClient(gatewayURL, "orion-svc-token", nil),
		DataSources: map[string]effects.DataSource{
			rankingDatasource: {Name: rankingDatasource, Svc: rankingDatasource},
			truthDatasource:   {Name: truthDatasource, Svc: truthDatasource},
		},
	}
}

// truthCall records one /truth/_query hit: the id its where clause carried.
type truthCall struct {
	whereID string
}

// namedGateway is the stub ZabGate: /ranking/_query returns the top-5;
// /truth/_query parses the where[0].value id and returns the matching
// player. It records the ordered sequence of truth calls so the test can
// assert they fired sequentially, each with the right id_k.
func namedGateway(t *testing.T, truthCalls *[]truthCall, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ranking/api/v1/_query":
			_, _ = w.Write([]byte(namedRankingRows()))
		case "/truth/api/v1/_query":
			var desc struct {
				Table string `json:"table"`
				Where []struct {
					Column string `json:"column"`
					Op     string `json:"op"`
					Value  string `json:"value"`
				} `json:"where"`
				Select []string `json:"select"`
			}
			var body strings.Builder
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			body.Write(buf)
			if err := json.Unmarshal([]byte(body.String()), &desc); err != nil {
				t.Errorf("truth query: undecodable descriptor: %v (%q)", err, body.String())
				http.Error(w, "bad descriptor", http.StatusBadRequest)
				return
			}
			if desc.Table != "players" || len(desc.Where) != 1 ||
				desc.Where[0].Column != "id" || desc.Where[0].Op != "=" {
				t.Errorf("truth query: malformed descriptor: %+v", desc)
				http.Error(w, "bad descriptor", http.StatusBadRequest)
				return
			}
			id := desc.Where[0].Value
			mu.Lock()
			*truthCalls = append(*truthCalls, truthCall{whereID: id})
			mu.Unlock()
			name, ok := playerNames[id]
			if !ok {
				// Unknown id → empty rows.
				_, _ = w.Write([]byte(`{"rows":[],"count":0,"elapsed_ms":1}`))
				return
			}
			// The real top-5 carry a NULL display_name; only summoner_name is
			// filled. Returning display_name:null here proves the board maps
			// summoner_name positionally — a regression to display_name would
			// render blank pseudos and fail the expectedLeaderboard assertion.
			fmt.Fprintf(w, `{"rows":[{"display_name":null,"summoner_name":%q}],"count":1,"elapsed_ms":1}`, name)
		default:
			t.Errorf("unexpected gateway path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

// TestE2E_NamedLeaderboard_FirstFlight flies the M3 named leaderboard end
// to end: push → validate → activate → on-start fires the ranking query
// then FIVE SEQUENTIAL truth queries, each resolving ranking_rows[k] to a
// pseudo; the final leaderboard_display maps every rank to its real name;
// AND the M1 reactive chat repaints chat.display live.
func TestE2E_NamedLeaderboard_FirstFlight(t *testing.T) {
	st := requireDB(t)

	var truthCalls []truthCall
	var mu sync.Mutex
	gw := namedGateway(t, &truthCalls, &mu)
	defer gw.Close()

	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "m3-named-leaderboard"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, namedLeaderboardFetcher())
	show.SetEffects(namedLeaderboardEffects(t, gw.URL))
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	if code, body := operatorPost(t, base+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"bp-live-data-named"}`); code != 200 {
		t.Fatalf("push of named-leaderboard scene = %d %v", code, body)
	}

	// #87 gate: activate-before-validate refused.
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != http.StatusConflict ||
		body["code"] != "SCENE_NOT_VALIDATED" {
		t.Fatalf("activate-before-validate = %d %v, want 409 SCENE_NOT_VALIDATED", code, body)
	}

	if code, _ := operatorPost(t, base+"/validate", `{}`); code != http.StatusAccepted {
		t.Fatalf("validate = %d", code)
	}
	waitValidated(t, base)
	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != 200 {
		t.Fatalf("activate-after-validate = %d %v", code, body)
	}

	// Proof 1 (the M3 criterion): the static unroll composed the full
	// named leaderboard onto the exec leaf with the REAL mapped pseudos.
	want := string(mustJSON(expectedLeaderboard()))
	assertLiveDataLeaf(t, show, leaderboardDisplayLeaf, func(v string) bool {
		return v == want
	})

	// Proof 2: exactly five truth queries fired, SEQUENTIALLY, each
	// carrying the correct id_k positionally (rank order).
	mu.Lock()
	calls := append([]truthCall(nil), truthCalls...)
	mu.Unlock()
	if len(calls) != namedTopN {
		t.Fatalf("truth queries fired = %d, want %d (sequence: %+v)", len(calls), namedTopN, calls)
	}
	for k, c := range calls {
		if c.whereID != playerIDsByRank[k] {
			t.Fatalf("truth query #%d carried id %q, want %q (out-of-order or wrong positional map)",
				k, c.whereID, playerIDsByRank[k])
		}
	}

	// Proof 3 (the M1 reactive guard): a real-shaped chat event repaints
	// chat.display live, alongside the five-query data chain.
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

// TestE2E_NamedLeaderboard_UnvalidatedSceneNeverQueries (R9): a pushed-
// but-NOT-validated named scene never airs, so NEITHER the ranking query
// NOR any of the five truth queries can fire — the stub gateway is never
// hit. The negative half of the data criterion.
func TestE2E_NamedLeaderboard_UnvalidatedSceneNeverQueries(t *testing.T) {
	st := requireDB(t)

	var gatewayHit atomic.Bool
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gatewayHit.Store(true)
		_, _ = w.Write([]byte(`{"rows":[],"count":0,"elapsed_ms":1}`))
	}))
	defer gw.Close()

	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "m3-named-gated"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, namedLeaderboardFetcher())
	show.SetEffects(namedLeaderboardEffects(t, gw.URL))
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()

	if code, _ := operatorPost(t, base+"/push",
		`{"canvas_version":"v1","blue_blueprint_id":"bp-live-data-named"}`); code != 200 {
		t.Fatalf("push = %d", code)
	}

	if code, body := operatorPost(t, srv.URL+"/api/v1/show/active-scene",
		`{"scene_id":"`+sceneID.String()+`"}`); code != http.StatusConflict ||
		body["code"] != "SCENE_NOT_VALIDATED" {
		t.Fatalf("activate of unvalidated scene = %d %v, want 409 SCENE_NOT_VALIDATED", code, body)
	}

	time.Sleep(150 * time.Millisecond)
	if gatewayHit.Load() {
		t.Fatal("R9 VIOLATION: an unvalidated named-leaderboard scene reached the _query gateway")
	}
	if a := show.Active(); a != nil {
		t.Fatalf("an unvalidated scene became active: %q", a.Graph().SceneVersion)
	}
}

// TestE2E_NamedLeaderboard_BindsSixQueries is the structural guard: the
// scene binds exactly six db.query effects (1 ranking + 5 truth) — the
// static unroll. Proven by construction so the named claim cannot regress.
func TestE2E_NamedLeaderboard_BindsSixQueries(t *testing.T) {
	g := namedLeaderboardBlueprint()
	var ranking, truth int
	for _, n := range g.Nodes {
		if n.Compute != "core.db.query@1" {
			continue
		}
		var ds string
		_ = json.Unmarshal(n.Config["datasource"], &ds)
		switch ds {
		case rankingDatasource:
			ranking++
		case truthDatasource:
			truth++
		}
	}
	if ranking != 1 || truth != namedTopN {
		t.Fatalf("db.query set = %d ranking + %d truth, want 1 + %d", ranking, truth, namedTopN)
	}
}
