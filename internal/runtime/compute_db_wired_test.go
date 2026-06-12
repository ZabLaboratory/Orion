package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// TestDBFromWired_TableThreadsToQueryBody is the regression guard for the
// live db `from`-table bug: a `core.db.from@1` node (config `table`, NO
// upstream — it is the chain head) wired into `core.db.query@1`'s
// `descriptor` input must thread its `table` all the way into the body
// Orion POSTs to `${gateway}/<svc>/api/v1/_query`.
//
// The bug: the compiler classified a no-upstream `from` node as
// kind="input" (the adapter-leaf heuristic), which (a) dropped its config
// — config is carried for `computed` nodes only — and (b) made the runtime
// treat it as adapter-written, so dbFromFn never ran. The descriptor that
// reached `_query` therefore had `table:""` → ZabRanking answered 422
// `string_too_short` on body.table → 0 rows on air, even though the pure
// builder fn and the descriptor SHAPE were correct (the parity tests
// passed). The fix classifies a chain-head KindCompute (`from`) as
// `computed` so its config is carried and the builder runs.
//
// This test compiles the wired graph through the REAL compiler, runs it
// through a live Scene, and asserts the body the DB client sends carries
// the non-empty table — exactly the threading the live exposed as broken.
func TestDBFromWired_TableThreadsToQueryBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"rows":[{"player":"GIDEON"}],"count":1,"elapsed_ms":2}`))
	}))
	defer srv.Close()

	ep := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "exec", Kind: "exec"}
	}
	dp := func(n string) compiler.BlueprintPort {
		return compiler.BlueprintPort{Name: n, Type: "any", Kind: "data"}
	}

	// on-start → query(exec_in); from.plan → query.descriptor (data edge).
	// `from` has NO upstream — the exact shape the heuristic misclassified.
	bp := &compiler.BlueprintGraph{
		ID: "bp-from-wired",
		Nodes: []compiler.BlueprintNode{
			{ID: "start", Compute: "core.event.on-start@1",
				Outputs: []compiler.BlueprintPort{ep("then")}},
			{ID: "from", Compute: "core.db.from@1",
				Config:  map[string]json.RawMessage{"table": raw(`"player_scores"`)},
				Outputs: []compiler.BlueprintPort{dp("plan")}},
			{ID: "q", Compute: "core.db.query@1",
				Config:  map[string]json.RawMessage{"datasource": raw(`"ranking"`)},
				Inputs:  []compiler.BlueprintPort{ep("exec_in"), dp("descriptor")},
				Outputs: []compiler.BlueprintPort{ep("then"), ep("error"), dp("rows"), dp("count")}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "start", FromPort: "then", ToNode: "q", ToPort: "exec_in"},
			{FromNode: "from", FromPort: "plan", ToNode: "q", ToPort: "descriptor"},
		},
	}
	fetcher := &stubFetcher{
		layout:    &compiler.CanvasLayout{Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		blueprint: bp,
		manifest: compiler.ComputeManifest{
			"core.event.on-start@1": {IsPure: true, IsBounded: true, Version: "1"},
			"core.db.from@1":        {IsPure: true, IsBounded: true, Version: "1"},
			"core.db.query@1":       {IsPure: false, IsBounded: false, Version: "1"},
		},
	}

	graph, bundle, _, err := compiler.Compile(context.Background(), "from-wired-scene",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-from-wired"}, fetcher)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// REGRESSION ASSERTION #1 (compile-time): the `from` data node must be
	// classified `computed` (so its config rides the artefact + the builder
	// runs), NOT `input`. This is the precise classification the bug got
	// wrong; assert it directly so a re-break is caught even without the DB.
	var fromNode *compiler.GraphNode
	for i := range graph.Nodes {
		if graph.Nodes[i].Compute == "core.db.from@1" {
			fromNode = &graph.Nodes[i]
		}
	}
	if fromNode == nil {
		t.Fatal("compiled graph has no core.db.from@1 data node")
	}
	if fromNode.Kind != "computed" {
		t.Fatalf("from node Kind = %q, want \"computed\" (the input-misclassification regression)", fromNode.Kind)
	}
	if string(fromNode.Config["table"]) != `"player_scores"` {
		t.Fatalf("from node dropped its config.table: %v", fromNode.Config)
	}

	progs, err := ExecProgramsFromGraph(graph)
	if err != nil {
		t.Fatalf("exec programs: %v", err)
	}

	sc := NewScene("from-wired-scene", graph, bundle, NewComputeRegistry(), quietLogger())
	sc.InstallExec(progs...)
	sc.SetEffects(&SceneEffects{
		Runner:      newTestRunner(t),
		DB:          effects.NewDBQueryClient(srv.URL, "orion-token", nil),
		DataSources: map[string]effects.DataSource{"ranking": {Name: "ranking", Svc: "ranking"}},
	})
	startScene(t, sc)

	// on-start fires the entry; the query parks, the worker POSTs the body.
	fireAllOnStart(t, sc, progs[0])

	// REGRESSION ASSERTION #2 (runtime): the body that reached `_query`
	// carries the non-empty table threaded from `from`.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gotBody != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gotBody == "" {
		t.Fatal("_query never received a request body")
	}
	var sent struct {
		Table string `json:"table"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("query body not JSON: %v (%s)", err, gotBody)
	}
	if sent.Table != "player_scores" {
		t.Fatalf("table did not thread from→query: body.table = %q, want \"player_scores\" (the live 422 string_too_short bug)", sent.Table)
	}
}

// fireAllOnStart fires every on-start entrypoint of the program.
func fireAllOnStart(t *testing.T, sc *Scene, prog *ExecProgram) {
	t.Helper()
	fired := false
	for id, e := range prog.Entrypoints {
		if e.Kind == EntryOnStart {
			mustFire(t, sc, id)
			fired = true
		}
	}
	if !fired {
		t.Fatal("no on-start entrypoint to fire")
	}
}
