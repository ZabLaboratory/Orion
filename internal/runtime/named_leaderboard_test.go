package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// Milestone 3 — the NAMED leaderboard, proven DB-FREE at the runtime
// layer (the e2e twin in tests/e2e/named_leaderboard_test.go flies the
// SAME graph through the real push API + live PG). This test compiles the
// static-unroll blueprint, installs its exec program + a stub _query
// gateway, fires on-start, and proves:
//
//   - the five db.query(truth) effects fire SEQUENTIALLY, each carrying
//     ranking_rows[k].player_id in its where clause (positional mapping);
//   - the descriptor-from-data construction (literal base + set-field +
//     list-append) yields a valid {table:players, where:[{id = id_k}]};
//   - the folded leaderboard_display maps every rank to its real pseudo.
//
// No loop, no dynamic join: the third way the maintainer green-lit.

const (
	nlbRankingDS = "ranking"
	nlbTruthDS   = "truth"
	nlbTopN      = 5
)

var nlbIDs = []string{
	"11111111-1111-1111-1111-111111111111",
	"22222222-2222-2222-2222-222222222222",
	"33333333-3333-3333-3333-333333333333",
	"44444444-4444-4444-4444-444444444444",
	"55555555-5555-5555-5555-555555555555",
}

// summoner_names (the LoL in-game name, NOT NULL) the stub /truth/_query
// returns per id — the displayed pseudo reads this column, not the
// nullable display_name. The real top-5 in-game names.
var nlbNames = []string{"GIDEON", "Teddy", "Loki", "Gumayusi", "Namgung"}
var nlbScores = []string{"9", "8", "7", "6", "4"}

func nlbRankingResponse() string {
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for k := 0; k < nlbTopN; k++ {
		if k > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"player_id":%q,"score":%s}`, nlbIDs[k], nlbScores[k])
	}
	fmt.Fprintf(&b, `],"count":%d,"elapsed_ms":2}`, nlbTopN)
	return b.String()
}

func nlbExpectedBoard() string {
	lines := make([]string, nlbTopN)
	for k := 0; k < nlbTopN; k++ {
		lines[k] = fmt.Sprintf("%d. %s — %s", k+1, nlbNames[k], nlbScores[k])
	}
	return strings.Join(lines, "\n")
}

func nlbLit(id string, v json.RawMessage) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.literal@1",
		Config:  map[string]json.RawMessage{"value": v},
		Outputs: []compiler.BlueprintPort{{Name: "out", Type: "any", Kind: "data"}}}
}

func nlbStr(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

func nlbGetField(id, path string) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.data.get-field@1",
		Config:  map[string]json.RawMessage{"path": nlbStr(path)},
		Inputs:  []compiler.BlueprintPort{{Name: "record", Type: "any", Kind: "data"}},
		Outputs: []compiler.BlueprintPort{{Name: "value", Type: "any", Kind: "data"}}}
}

// nlbInput reads a state leaf (here a __vars..<name> leaf an exec
// variable.set wrote). core.input resolves its leaf via nodeLeafPath
// (config name) — unlike core.variable.get, which has no data-layer leaf
// and never resolves in the demand cone.
func nlbInput(id, leaf string) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.input@1",
		Config:  map[string]json.RawMessage{"name": nlbStr(leaf)},
		Outputs: []compiler.BlueprintPort{{Name: "out", Type: "any", Kind: "data"}}}
}

func nlbConcat(id string) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.string.concat@1",
		Inputs:  []compiler.BlueprintPort{{Name: "a", Type: "any", Kind: "data"}, {Name: "b", Type: "any", Kind: "data"}},
		Outputs: []compiler.BlueprintPort{{Name: "out", Type: "any", Kind: "data"}}}
}

func ePin(name string) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: name, Type: "exec", Kind: "exec"}
}
func dPin(name string) compiler.BlueprintPort {
	return compiler.BlueprintPort{Name: name, Type: "any", Kind: "data"}
}

func nlbBlueprint() *compiler.BlueprintGraph {
	g := &compiler.BlueprintGraph{ID: "bp-live-data-named"}
	add := func(n ...compiler.BlueprintNode) { g.Nodes = append(g.Nodes, n...) }
	edge := func(fn, fp, tn, tp string) {
		g.Edges = append(g.Edges, compiler.BlueprintEdge{FromNode: fn, FromPort: fp, ToNode: tn, ToPort: tp})
	}

	// ranking tranche
	add(compiler.BlueprintNode{ID: "start", Compute: "core.event.on-start@1",
		Outputs: []compiler.BlueprintPort{ePin("then")}})
	add(compiler.BlueprintNode{ID: "qRank", Compute: "core.db.query@1",
		Config: map[string]json.RawMessage{
			"datasource": nlbStr(nlbRankingDS),
			"descriptor": json.RawMessage(`{"table":"player_scores","select":["player_id","score"],"order":[{"column":"score","direction":"desc"}],"limit":5}`),
		},
		Inputs:  []compiler.BlueprintPort{ePin("exec_in")},
		Outputs: []compiler.BlueprintPort{ePin("then"), ePin("error"), dPin("rows"), dPin("count")}})
	add(compiler.BlueprintNode{ID: "setRows", Compute: "core.variable.set@1",
		Config:  map[string]json.RawMessage{"variable": nlbStr("ranking_rows")},
		Inputs:  []compiler.BlueprintPort{ePin("exec_in"), dPin("value")},
		Outputs: []compiler.BlueprintPort{ePin("then")}})
	edge("start", "then", "qRank", "exec_in")
	edge("qRank", "then", "setRows", "exec_in")
	edge("qRank", "rows", "setRows", "value")

	// ranking_rows read back as a state-leaf input (the demand cone reads
	// state leaves through core.input; an exec data-out pin would lose its
	// from_port through demandValue's pure-input recursion).
	add(nlbInput("inRows", "__vars..ranking_rows"))

	for k := 0; k < nlbTopN; k++ {
		s := func(b string) string { return fmt.Sprintf("%s%d", b, k) }
		// id_k / score_k read ranking_rows[k] (STATIC config path) off the
		// __vars..ranking_rows state leaf via the inRows input node.
		add(nlbGetField(s("getId"), fmt.Sprintf("%d.player_id", k)))
		add(nlbGetField(s("getScore"), fmt.Sprintf("%d.score", k)))
		edge("inRows", "out", s("getId"), "record")
		edge("inRows", "out", s("getScore"), "record")

		add(nlbLit(s("clauseLit"), json.RawMessage(`{"column":"id","op":"="}`)))
		add(compiler.BlueprintNode{ID: s("clause"), Compute: "core.data.set-field@1",
			Config:  map[string]json.RawMessage{"path": nlbStr("value")},
			Inputs:  []compiler.BlueprintPort{dPin("record"), dPin("value")},
			Outputs: []compiler.BlueprintPort{dPin("out")}})
		edge(s("clauseLit"), "out", s("clause"), "record")
		edge(s("getId"), "value", s("clause"), "value")

		add(nlbLit(s("whereEmpty"), json.RawMessage(`[]`)))
		add(compiler.BlueprintNode{ID: s("where"), Compute: "core.data.list-append@1",
			Inputs:  []compiler.BlueprintPort{dPin("list"), dPin("element")},
			Outputs: []compiler.BlueprintPort{dPin("out")}})
		edge(s("whereEmpty"), "out", s("where"), "list")
		edge(s("clause"), "out", s("where"), "element")

		add(nlbLit(s("descLit"), json.RawMessage(`{"table":"players","select":["display_name","summoner_name"]}`)))
		add(compiler.BlueprintNode{ID: s("desc"), Compute: "core.data.set-field@1",
			Config:  map[string]json.RawMessage{"path": nlbStr("where")},
			Inputs:  []compiler.BlueprintPort{dPin("record"), dPin("value")},
			Outputs: []compiler.BlueprintPort{dPin("out")}})
		edge(s("descLit"), "out", s("desc"), "record")
		edge(s("where"), "out", s("desc"), "value")

		add(compiler.BlueprintNode{ID: s("query"), Compute: "core.db.query@1",
			Config:  map[string]json.RawMessage{"datasource": nlbStr(nlbTruthDS)},
			Inputs:  []compiler.BlueprintPort{ePin("exec_in"), dPin("descriptor")},
			Outputs: []compiler.BlueprintPort{ePin("then"), ePin("error"), dPin("rows"), dPin("count")}})
		edge(s("desc"), "out", s("query"), "descriptor")

		// query_k.then → variable.set(row_k = query_k.rows) so the pseudo
		// extractor reads the returned row off a state leaf (same reason as
		// inRows). setRow_k pulls query_k.rows on its exec value pin (port
		// preserved through pullData).
		add(compiler.BlueprintNode{ID: s("setRow"), Compute: "core.variable.set@1",
			Config:  map[string]json.RawMessage{"variable": nlbStr(fmt.Sprintf("row_%d", k))},
			Inputs:  []compiler.BlueprintPort{ePin("exec_in"), dPin("value")},
			Outputs: []compiler.BlueprintPort{ePin("then")}})
		edge(s("query"), "rows", s("setRow"), "value")

		add(nlbInput(s("inRow"), fmt.Sprintf("__vars..row_%d", k)))
		// the pseudo reads summoner_name (NOT NULL; the LoL in-game name) —
		// display_name is nullable and the real top-5 carry a NULL one.
		add(nlbGetField(s("name"), "0.summoner_name"))
		edge(s("inRow"), "out", s("name"), "record")

		add(nlbLit(s("rankLit"), nlbStr(fmt.Sprintf("%d. ", k+1))))
		add(nlbLit(s("dashLit"), nlbStr(" — ")))
		add(compiler.BlueprintNode{ID: s("scoreStr"), Compute: "core.cast.to-string@1",
			Inputs:  []compiler.BlueprintPort{dPin("value")},
			Outputs: []compiler.BlueprintPort{dPin("out")}})
		edge(s("getScore"), "value", s("scoreStr"), "value")
		add(nlbConcat(s("catA")))
		add(nlbConcat(s("catB")))
		add(nlbConcat(s("line")))
		edge(s("rankLit"), "out", s("catA"), "a")
		edge(s("name"), "value", s("catA"), "b")
		edge(s("catA"), "out", s("catB"), "a")
		edge(s("dashLit"), "out", s("catB"), "b")
		edge(s("catB"), "out", s("line"), "a")
		edge(s("scoreStr"), "out", s("line"), "b")
	}

	// Join line_0..line_4 with "\n" — each line_k pure cone reads its row_k
	// back off the __vars..row_k state leaf (via inRow_k), so the folded
	// board recomputes reactively as each row lands (boardOut below).
	add(nlbLit("nl", nlbStr("\n")))
	prev := "line0"
	for k := 1; k < nlbTopN; k++ {
		sepCat := fmt.Sprintf("joinSep%d", k)
		lineCat := fmt.Sprintf("joinLine%d", k)
		add(nlbConcat(sepCat))
		add(nlbConcat(lineCat))
		edge(prev, "out", sepCat, "a")
		edge("nl", "out", sepCat, "b")
		edge(sepCat, "out", lineCat, "a")
		edge(fmt.Sprintf("line%d", k), "out", lineCat, "b")
		prev = lineCat
	}
	// the board is a REACTIVE dataflow output (core.output@1, passthrough):
	// it recomputes its leaf whenever ANY upstream leaf in its cone changes —
	// i.e. every time a truth query lands its row_k via setRow_k. This makes
	// the named board DETERMINISTIC and independent of exec-spine completion
	// ordering. The previous one-shot variable.set demand-pulled the join
	// cone at a single instant (the spine tail) and assumed every row_k leaf
	// was already populated — a fragile coupling that left blank pseudos when
	// the assumption did not hold in prod.
	add(compiler.BlueprintNode{ID: "boardOut", Compute: "core.output@1",
		Config: map[string]json.RawMessage{"name": nlbStr("__vars..leaderboard_display")},
		Inputs: []compiler.BlueprintPort{dPin("value")}})
	edge(prev, "out", "boardOut", "value")

	// exec spine: setRows → query0 → setRow0 → query1 → … → query4 → setRow4.
	// Each query parks/resumes before the next: five SEQUENTIAL dependent
	// effects, zero fork. The spine ENDS at setRow4 — it only fires the six
	// queries and lands each row_k leaf; the board is reactive (boardOut).
	edge("setRows", "then", "query0", "exec_in")
	for k := 0; k < nlbTopN; k++ {
		edge(fmt.Sprintf("query%d", k), "then", fmt.Sprintf("setRow%d", k), "exec_in")
		if k+1 < nlbTopN {
			edge(fmt.Sprintf("setRow%d", k), "then", fmt.Sprintf("query%d", k+1), "exec_in")
		}
	}
	return g
}

func nlbManifest() compiler.ComputeManifest {
	return compiler.ComputeManifest{
		"core.event.on-start@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.db.query@1":         {IsPure: false, IsBounded: false, Version: "1"},
		"core.variable.set@1":     {IsPure: true, IsBounded: true, Version: "1"},
		"core.variable.get@1":     {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.get-field@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.set-field@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.list-append@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.literal@1":          {IsPure: true, IsBounded: true, Version: "1"},
		"core.cast.to-string@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.string.concat@1":    {IsPure: true, IsBounded: true, Version: "1"},
		"core.input@1":            {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":           {IsPure: true, IsBounded: true, Version: "1"},
	}
}

// TestNamedLeaderboard_StaticUnroll_RunsLive is the DB-free flight proof.
func TestNamedLeaderboard_StaticUnroll_RunsLive(t *testing.T) {
	var truthIDs []string
	var mu sync.Mutex
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ranking/api/v1/_query":
			_, _ = w.Write([]byte(nlbRankingResponse()))
		case "/truth/api/v1/_query":
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			var desc struct {
				Table string `json:"table"`
				Where []struct {
					Column string `json:"column"`
					Op     string `json:"op"`
					Value  string `json:"value"`
				} `json:"where"`
			}
			if err := json.Unmarshal(buf, &desc); err != nil {
				t.Errorf("truth descriptor undecodable: %v (%q)", err, buf)
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			if desc.Table != "players" || len(desc.Where) != 1 ||
				desc.Where[0].Column != "id" || desc.Where[0].Op != "=" {
				t.Errorf("truth descriptor malformed: %+v", desc)
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			id := desc.Where[0].Value
			mu.Lock()
			truthIDs = append(truthIDs, id)
			mu.Unlock()
			name := ""
			for i, known := range nlbIDs {
				if known == id {
					name = nlbNames[i]
				}
			}
			// display_name:null mirrors the real top-5 (only summoner_name is
			// filled); the board must map summoner_name positionally.
			fmt.Fprintf(w, `{"rows":[{"display_name":null,"summoner_name":%q}],"count":1,"elapsed_ms":1}`, name)
		default:
			t.Errorf("unexpected gateway path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer gw.Close()

	// Compile the real artefact the push handler would persist.
	fetcher := &stubFetcher{
		layout:    &compiler.CanvasLayout{Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		blueprint: nlbBlueprint(),
		manifest:  nlbManifest(),
	}
	graph, bundle, version, err := compiler.Compile(context.Background(), "m3-named",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-live-data-named"}, fetcher)
	if err != nil {
		t.Fatalf("named-leaderboard graph failed to compile: %v", err)
	}
	if version == "" {
		t.Fatal("compile minted an empty scene_version")
	}

	progs, err := ExecProgramsFromGraph(graph)
	if err != nil {
		t.Fatalf("named graph did not emit an ExecProgram: %v", err)
	}

	sc := NewScene("m3-named", graph, bundle, NewComputeRegistry(), quietLogger())
	for _, p := range progs {
		sc.InstallExec(p)
	}
	runner := effects.NewRunner(4, 64, quietLogger())
	runner.Start()
	t.Cleanup(runner.Stop)
	sc.SetEffects(&SceneEffects{
		Runner: runner,
		Egress: effects.NewEgressPolicy(nil, false),
		DB:     effects.NewDBQueryClient(gw.URL, "orion-svc-token", nil),
		DataSources: map[string]effects.DataSource{
			nlbRankingDS: {Name: nlbRankingDS, Svc: nlbRankingDS},
			nlbTruthDS:   {Name: nlbTruthDS, Svc: nlbTruthDS},
		},
	})
	startScene(t, sc)

	startEntry := ""
	for _, p := range progs {
		for name, e := range p.Entrypoints {
			if e.Kind == EntryOnStart {
				startEntry = name
			}
		}
	}
	if startEntry == "" {
		t.Fatal("no resolvable on-start entry")
	}
	mustFire(t, sc, startEntry)

	// Proof 1: the folded named leaderboard landed, real pseudos mapped.
	want, _ := json.Marshal(nlbExpectedBoard())
	waitForState(t, sc, "__vars..leaderboard_display", string(want), 5*time.Second)

	// Proof 2: exactly five truth queries, SEQUENTIAL, correct id per rank.
	mu.Lock()
	got := append([]string(nil), truthIDs...)
	mu.Unlock()
	if len(got) != nlbTopN {
		t.Fatalf("truth queries = %d, want %d (%v)", len(got), nlbTopN, got)
	}
	for k, id := range got {
		if id != nlbIDs[k] {
			t.Fatalf("truth query #%d id = %q, want %q (out-of-order / wrong positional map)", k, id, nlbIDs[k])
		}
	}
	t.Logf("leaderboard_display =\n%s", nlbExpectedBoard())
}
