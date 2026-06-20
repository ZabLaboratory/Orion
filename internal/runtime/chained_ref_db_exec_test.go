package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// Chained-reference exec-splice runtime regression (Forge, ADR 014, follow-up
// to #189/#190). The live symptom (Keeper repro on scene 5aa129e5, event `lck`):
// a chain of chained Blue references
//
//	byLeague(match-by-league).query  →  matchIdSet(variable.set __vars..match_id)
//	  →  roster(match-roster).query  →  rowsSet(variable.set roster_rows)
//
// runs byLeague.query fine (rows + match_id derived), but roster.query — whose
// descriptor depends on the match_id byLeague produced — never fires at runtime,
// so roster_rows stays empty and the 10 downstream score lookups read null.
//
// This test reproduces the MINIMAL faithful shape with the REAL compiler and a
// live Scene driving REAL core.db.query@1 effects against an httptest `_query`
// endpoint that RECORDS every query body. The discriminating assertion: the
// SECOND (roster) query must reach `_query`, and its WHERE clause must carry the
// match_id the FIRST (byLeague) query produced. If the inter-reference exec edge
// (ref1 → matchIdSet → ref2.query) is not spliced/reached, the second body never
// arrives and the test fails — exactly the live trou.

// recordingDB is an httptest `_query` server recording every descriptor body it
// receives (one per db.query effect), keyed in arrival order.
type recordingDB struct {
	mu     sync.Mutex
	bodies []string
	// rowsFor maps a table name → the JSON rows array the server answers with.
	rowsFor map[string]string
}

func (d *recordingDB) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	d.mu.Lock()
	d.bodies = append(d.bodies, body)
	d.mu.Unlock()

	var sent struct {
		Table string `json:"table"`
	}
	_ = json.Unmarshal(b, &sent)
	rows := d.rowsFor[sent.Table]
	if rows == "" {
		rows = "[]"
	}
	_, _ = w.Write([]byte(`{"rows":` + rows + `,"count":1,"elapsed_ms":1}`))
}

func (d *recordingDB) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.bodies))
	copy(out, d.bodies)
	return out
}

// inNode / outNode build interface relay nodes (core.input@1 / core.output@1)
// for a referenced function's interface, carrying the pin name in config.name —
// the same shape Blue seeds (the compiler-package helpers live in that package).
func inNode(id, name string) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.input@1",
		Config: map[string]json.RawMessage{"name": json.RawMessage(`"` + name + `"`)}}
}

func outNode(id, name string) compiler.BlueprintNode {
	return compiler.BlueprintNode{ID: id, Compute: "core.output@1",
		Config: map[string]json.RawMessage{"name": json.RawMessage(`"` + name + `"`)}}
}

// byLeagueRefGraph models the FIRST referenced function (match-by-league):
//
//	exec_in → q(db.query "matches") → setRows(variable.set league_rows)
//	          → leagueOut(core.output "then", exec)
//	rows → getId(get-field "0.id") → idOut(core.output "match_id", data)
//
// Its db.query is driven by the caller spine; on `then` it both finishes the
// exec spine (so the chain continues) AND exposes match_id (row 0's id) as a
// data output pin the scene threads into roster.
func byLeagueRefGraph() *compiler.ResolvedBlueprintGraph {
	leafRows := "__vars..league_rows"
	dp := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "any", Kind: "data"}
	}
	ep := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
	}
	return &compiler.ResolvedBlueprintGraph{
		BlueprintID: "bp-by-league",
		Version:     1,
		Nodes: []compiler.BlueprintNode{
			inNode("ein", "exec_in"),
			{ID: "from", Compute: "core.db.from@1",
				Config:  map[string]json.RawMessage{"table": json.RawMessage(`"matches"`)},
				Outputs: []compiler.BlueprintPort{dp("plan")}},
			{ID: "q", Compute: "core.db.query@1",
				Config:  map[string]json.RawMessage{"datasource": json.RawMessage(`"truth"`)},
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("descriptor")},
				Outputs: []compiler.BlueprintPort{ep("then"), ep("error"), dp("rows")}},
			{ID: "setRows", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"league_rows"`)},
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("value")},
				Outputs: []compiler.BlueprintPort{ep("then")}},
			// Reader of the rows the set just wrote (the production shape: getId
			// reads __vars..league_rows, not the live env pin).
			{ID: "inRows", Compute: "core.input@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"` + leafRows + `"`)},
				Outputs: []compiler.BlueprintPort{dp("value")}},
			{ID: "getId", Compute: "core.data.get-field@1",
				Config:  map[string]json.RawMessage{"path": json.RawMessage(`"0.id"`)},
				Inputs:  []compiler.BlueprintPort{dp("record")},
				Outputs: []compiler.BlueprintPort{dp("value")}},
			outNode("leagueOut", "then"),
			outNode("idOut", "match_id"),
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "from", FromPort: "plan", ToNode: "q", ToPort: "descriptor"},
			{FromNode: "ein", FromPort: "then", ToNode: "q", ToPort: "exec_in"},
			{FromNode: "q", FromPort: "then", ToNode: "setRows", ToPort: "exec_in"},
			{FromNode: "q", FromPort: "rows", ToNode: "setRows", ToPort: "value"},
			{FromNode: "setRows", FromPort: "then", ToNode: "leagueOut", ToPort: "value"},
			{FromNode: "inRows", FromPort: "value", ToNode: "getId", ToPort: "record"},
			{FromNode: "getId", FromPort: "value", ToNode: "idOut", ToPort: "value"},
		},
		Interface: compiler.BlueprintInterface{
			Inputs: []compiler.BlueprintInterfacePin{
				{Name: "exec_in", Type: "exec", Kind: "exec", Required: true},
			},
			Outputs: []compiler.BlueprintInterfacePin{
				{Name: "then", Type: "exec", Kind: "exec"},
				{Name: "match_id", Type: "any", Kind: "data"},
			},
		},
		Purity: compiler.BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

// rosterRefGraph models the SECOND referenced function (match-roster):
//
//	exec_in → q(db.query "match_players") → setRows(variable.set roster_rows)
//	          → rosterOut(core.output "then", exec)
//
// Its db.query descriptor carries a WHERE clause whose value is the match_id
// input pin — the value the scene threads from byLeague. This is the node that
// never fired live.
func rosterRefGraph() *compiler.ResolvedBlueprintGraph {
	dp := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "any", Kind: "data"}
	}
	ep := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
	}
	return &compiler.ResolvedBlueprintGraph{
		BlueprintID: "bp-roster",
		Version:     1,
		Nodes: []compiler.BlueprintNode{
			inNode("ein", "exec_in"),
			inNode("inMatchId", "match_id"),
			{ID: "from", Compute: "core.db.from@1",
				Config:  map[string]json.RawMessage{"table": json.RawMessage(`"match_players"`)},
				Outputs: []compiler.BlueprintPort{dp("plan")}},
			// where(plan, value=match_id, column="match_id") — descriptor depends
			// on the threaded match_id; this is the data the live SELECT showed.
			{ID: "where", Compute: "core.db.where@1",
				Config: map[string]json.RawMessage{"column": json.RawMessage(`"match_id"`),
					"op": json.RawMessage(`"="`)},
				Inputs:  []compiler.BlueprintPort{dp("plan"), dp("value")},
				Outputs: []compiler.BlueprintPort{dp("plan")}},
			{ID: "q", Compute: "core.db.query@1",
				Config:  map[string]json.RawMessage{"datasource": json.RawMessage(`"truth"`)},
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("descriptor")},
				Outputs: []compiler.BlueprintPort{ep("then"), ep("error"), dp("rows")}},
			{ID: "setRows", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"roster_rows"`)},
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("value")},
				Outputs: []compiler.BlueprintPort{ep("then")}},
			outNode("rosterOut", "then"),
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "from", FromPort: "plan", ToNode: "where", ToPort: "plan"},
			{FromNode: "inMatchId", FromPort: "value", ToNode: "where", ToPort: "value"},
			{FromNode: "where", FromPort: "plan", ToNode: "q", ToPort: "descriptor"},
			{FromNode: "ein", FromPort: "then", ToNode: "q", ToPort: "exec_in"},
			{FromNode: "q", FromPort: "then", ToNode: "setRows", ToPort: "exec_in"},
			{FromNode: "q", FromPort: "rows", ToNode: "setRows", ToPort: "value"},
			{FromNode: "setRows", FromPort: "then", ToNode: "rosterOut", ToPort: "value"},
		},
		Interface: compiler.BlueprintInterface{
			Inputs: []compiler.BlueprintInterfacePin{
				{Name: "exec_in", Type: "exec", Kind: "exec", Required: true},
				{Name: "match_id", Type: "any", Kind: "data"},
			},
			Outputs: []compiler.BlueprintInterfacePin{
				{Name: "then", Type: "exec", Kind: "exec"},
			},
		},
		Purity: compiler.BlueprintPurity{IsPure: true, IsBounded: true},
	}
}

// chainedRefScene wires the two references on one spine, with the scene-level
// matchIdSet between them (exactly the live exec program shape):
//
//	start.then → byLeague.exec_in
//	byLeague.then → matchIdSet.exec_in ; byLeague.match_id → matchIdSet.value
//	matchIdSet.then → roster.exec_in   ; matchIdSet writes __vars..match_id
//	roster.match_id ← __vars..match_id (read inside roster via its inMatchId pin,
//	  which the scene wires from a get reading the var)
func chainedRefScene() *compiler.BlueprintGraph {
	dp := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "any", Kind: "data"}
	}
	ep := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
	}
	return &compiler.BlueprintGraph{
		ID: "bp-scene",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{ep("then")}},
			{ID: "byLeague", Compute: "blueprint.reference",
				Reference: &compiler.BlueprintReference{BlueprintID: "bp-by-league", Version: 1}},
			// Scene-level variable.set sitting BETWEEN the two references — the
			// exact intermediate node the live `matchIdSet` is.
			{ID: "matchIdSet", Compute: "core.variable.set@1",
				Config:  map[string]json.RawMessage{"variable": json.RawMessage(`"match_id"`)},
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("value")},
				Outputs: []compiler.BlueprintPort{ep("then")}},
			// Reader of the match_id var, threaded into roster's match_id pin.
			{ID: "matchIdRead", Compute: "core.input@1",
				Config:  map[string]json.RawMessage{"name": json.RawMessage(`"__vars..match_id"`)},
				Outputs: []compiler.BlueprintPort{dp("value")}},
			{ID: "roster", Compute: "blueprint.reference",
				Reference: &compiler.BlueprintReference{BlueprintID: "bp-roster", Version: 1}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "byLeague", ToPort: "exec_in"},
			{FromNode: "byLeague", FromPort: "then", ToNode: "matchIdSet", ToPort: "exec_in"},
			{FromNode: "byLeague", FromPort: "match_id", ToNode: "matchIdSet", ToPort: "value"},
			{FromNode: "matchIdSet", FromPort: "then", ToNode: "roster", ToPort: "exec_in"},
			{FromNode: "matchIdRead", FromPort: "value", ToNode: "roster", ToPort: "match_id"},
		},
	}
}

func chainedRefManifest() compiler.ComputeManifest {
	return compiler.ComputeManifest{
		"core.event.on-start@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.input@1":          {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":         {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.get-field@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.variable.set@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.db.from@1":        {IsPure: true, IsBounded: true, Version: "1"},
		"core.db.where@1":       {IsPure: true, IsBounded: true, Version: "1"},
		"core.db.query@1":       {IsPure: false, IsBounded: false, Version: "1"},
	}
}

// TestChainedReference_SecondQueryReachesDBWithThreadedMatchID is the runtime
// proof of RC #1/#2: on a chain of two references where ref2's db.query
// descriptor depends on a value ref1's db.query produced, ref2's query MUST
// reach the DB and its WHERE must carry the threaded match_id.
func TestChainedReference_SecondQueryReachesDBWithThreadedMatchID(t *testing.T) {
	db := &recordingDB{rowsFor: map[string]string{
		// byLeague's matches query → one row with id = the match_id.
		"matches": `[{"id":"10eba940-aaaa-bbbb-cccc-ddddeeeeffff"}]`,
		// roster's match_players query → a roster row (only returned when it fires).
		"match_players": `[{"player_id":"p-namgung"}]`,
	}}
	srv := httptest.NewServer(http.HandlerFunc(db.handler))
	defer srv.Close()

	f := &refAwareFetcher{
		layout: &compiler.CanvasLayout{Version: "v1",
			Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		blueprint: chainedRefScene(),
		manifest:  chainedRefManifest(),
		graphs: map[string]*compiler.ResolvedBlueprintGraph{
			"bp-by-league@1": byLeagueRefGraph(),
			"bp-roster@1":    rosterRefGraph(),
		},
	}

	graph, bundle, version, err := compiler.Compile(context.Background(), "chained-ref-scene",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-scene"}, f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if version == "" {
		t.Fatal("empty scene_version")
	}

	progs, err := ExecProgramsFromGraph(graph)
	if err != nil {
		t.Fatalf("exec programs: %v", err)
	}

	sc := NewScene("chained-ref-scene", graph, bundle, NewComputeRegistry(), quietLogger())
	sc.InstallExec(progs...)
	sc.SetEffects(&SceneEffects{
		Runner:      newTestRunner(t),
		DB:          effects.NewDBQueryClient(srv.URL, "orion-token", nil),
		DataSources: map[string]effects.DataSource{"truth": {Name: "truth", Svc: "truth"}},
	})
	startScene(t, sc)
	fireAllOnStart(t, sc, progs[0])

	// Wait for BOTH queries to land. byLeague (matches) lands first; roster
	// (match_players) only lands if the inter-reference exec edge is spliced
	// and reached AND its descriptor sees the threaded match_id.
	var bodies []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		bodies = db.snapshot()
		if len(bodies) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	var sawMatches, sawRoster string
	for _, b := range bodies {
		var sent struct {
			Table string `json:"table"`
		}
		_ = json.Unmarshal([]byte(b), &sent)
		switch sent.Table {
		case "matches":
			sawMatches = b
		case "match_players":
			sawRoster = b
		}
	}

	if sawMatches == "" {
		t.Fatalf("byLeague.query (matches) never reached the DB — chain head broken; bodies=%v", bodies)
	}
	if sawRoster == "" {
		t.Fatalf("roster.query (match_players) NEVER reached the DB — the chained-reference "+
			"exec edge byLeague→matchIdSet→roster.query is not spliced/reached at runtime "+
			"(the live trou). bodies=%v", bodies)
	}

	// roster's WHERE must carry the match_id byLeague produced.
	var rosterDesc struct {
		Where []struct {
			Column string          `json:"column"`
			Value  json.RawMessage `json:"value"`
		} `json:"where"`
	}
	if err := json.Unmarshal([]byte(sawRoster), &rosterDesc); err != nil {
		t.Fatalf("roster body not JSON: %v (%s)", err, sawRoster)
	}
	if len(rosterDesc.Where) != 1 || rosterDesc.Where[0].Column != "match_id" {
		t.Fatalf("roster WHERE clause missing/wrong: %s", sawRoster)
	}
	if got := string(rosterDesc.Where[0].Value); got != `"10eba940-aaaa-bbbb-cccc-ddddeeeeffff"` {
		t.Fatalf("roster WHERE.match_id = %s, want the byLeague-produced id "+
			"(match_id not threaded across the reference boundary)", got)
	}
}
