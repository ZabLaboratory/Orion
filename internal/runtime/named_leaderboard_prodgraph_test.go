package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// TestNamedLeaderboard_ProdGraph_NamesResolve flies the ACTUAL authored
// JSON graph (Blue's pinned bp-live-data-named, copied verbatim into
// testdata) through the REAL compiler + runtime — NOT the hand-built Go
// twin nlbBlueprint(). The prod board showed blank pseudos; if the
// authored JSON diverges from the twin, the bug surfaces here.
func TestNamedLeaderboard_ProdGraph_NamesResolve(t *testing.T) {
	raw, err := os.ReadFile("testdata/m3_prod_graph.json")
	if err != nil {
		t.Fatalf("read prod graph: %v", err)
	}
	var bp compiler.BlueprintGraph
	if err := json.Unmarshal(raw, &bp); err != nil {
		t.Fatalf("decode prod graph: %v", err)
	}
	bp.ID = "bp-live-data-named"

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ranking/api/v1/_query":
			_, _ = w.Write([]byte(nlbRankingResponse()))
		case "/truth/api/v1/_query":
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			var desc struct {
				Where []struct {
					Value string `json:"value"`
				} `json:"where"`
			}
			_ = json.Unmarshal(buf, &desc)
			id := ""
			if len(desc.Where) == 1 {
				id = desc.Where[0].Value
			}
			name := ""
			for i, known := range nlbIDs {
				if known == id {
					name = nlbNames[i]
				}
			}
			fmt.Fprintf(w, `{"rows":[{"display_name":null,"summoner_name":%q}],"count":1,"elapsed_ms":1}`, name)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer gw.Close()

	// nlbManifest covers the named-leaderboard computes; the prod graph also
	// carries the M1 reactive chat tranche, so add its platform-event compute.
	manifest := nlbManifest()
	manifest["quasar.twitch.chat@1"] = compiler.ComputeManifestEntry{IsPure: true, IsBounded: true, Version: "1"}

	fetcher := &stubFetcher{
		layout:    &compiler.CanvasLayout{Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		blueprint: &bp,
		manifest:  manifest,
	}
	graph, bundle, version, err := compiler.Compile(context.Background(), "m3-named",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-live-data-named"}, fetcher)
	if err != nil {
		t.Fatalf("compile prod graph: %v", err)
	}
	t.Logf("prod graph scene_version = %s", version)

	progs, err := ExecProgramsFromGraph(graph)
	if err != nil {
		t.Fatalf("exec program: %v", err)
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
		t.Fatal("no on-start entry")
	}
	mustFire(t, sc, startEntry)

	want, _ := json.Marshal(nlbExpectedBoard())
	waitForState(t, sc, "__vars..leaderboard_display", string(want), 5*time.Second)
}
