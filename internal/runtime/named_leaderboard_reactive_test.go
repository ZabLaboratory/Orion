package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// Repro of the PROD board bug (Keeper snapshot 57dc631f / bp b5722c91):
// scores resolve, pseudos blank. The runtime twin passes with an INSTANT
// stub; prod has per-query latency. This drives the SAME graph with a
// truth stub that responds after a delay (the real network), to flush out
// any park/resume vs demand-pull ordering hazard the instant stub hides.
func TestNamedLeaderboard_SlowTruth_NamesStillResolve(t *testing.T) {
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
			time.Sleep(15 * time.Millisecond) // realistic per-query latency
			fmt.Fprintf(w, `{"rows":[{"display_name":null,"summoner_name":%q}],"count":1,"elapsed_ms":1}`, name)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer gw.Close()

	sc := buildNamedScene(t, gw.URL)
	startScene(t, sc)
	fireNamedStart(t, sc)

	want, _ := json.Marshal(nlbExpectedBoard())
	waitForState(t, sc, "__vars..leaderboard_display", string(want), 5*time.Second)
}

// Repro of the RE-PUSH path: the brief says it worked once, then a re-push
// of the SAME blueprint produced blank pseudos + a different scene_version.
// A fresh instance of the active scene fires on-start again — the board
// must resolve names on the SECOND flight too.
func TestNamedLeaderboard_RePush_NamesStillResolve(t *testing.T) {
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

	want, _ := json.Marshal(nlbExpectedBoard())
	for flight := 0; flight < 3; flight++ {
		sc := buildNamedScene(t, gw.URL)
		startScene(t, sc)
		fireNamedStart(t, sc)
		waitForState(t, sc, "__vars..leaderboard_display", string(want), 5*time.Second)
	}
}

// --- shared builders -------------------------------------------------

func buildNamedScene(t *testing.T, gwURL string) *Scene {
	t.Helper()
	fetcher := &stubFetcher{
		layout:    &compiler.CanvasLayout{Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		blueprint: nlbBlueprint(),
		manifest:  nlbManifest(),
	}
	graph, bundle, version, err := compiler.Compile(context.Background(), "m3-named",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-live-data-named"}, fetcher)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if version == "" {
		t.Fatal("empty scene_version")
	}
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
		DB:     effects.NewDBQueryClient(gwURL, "orion-svc-token", nil),
		DataSources: map[string]effects.DataSource{
			nlbRankingDS: {Name: nlbRankingDS, Svc: nlbRankingDS},
			nlbTruthDS:   {Name: nlbTruthDS, Svc: nlbTruthDS},
		},
	})
	return sc
}

func fireNamedStart(t *testing.T, sc *Scene) {
	t.Helper()
	progs, _ := ExecProgramsFromGraph(sc.graph)
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
}
