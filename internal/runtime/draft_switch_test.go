package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// Milestone 4 — the chat-driven DRAFT SWITCH, proven at the runtime layer
// against the ACTUAL authored Blue graph (testdata/m4_draft_switch_graph.json,
// byte-identical to Blue's bp-draft-switch fixture). This is the proof the
// brief asks for and the prod trap it warns about ("ça passe en test mais
// pas en prod"): the same JSON Orion fetches on a real push, run through the
// REAL compiler + runtime, with a stub /truth/_query that returns a
// DIFFERENT draft per match_id.
//
// The franchissement: a SINGLE chat message both
//   (1) repaints chat.display "<author>: <text>" (the M1 reactive guard), and
//   (2) CHANGES which draft is shown on draft.display.
// Two successive messages of DIFFERENT length select DIFFERENT drafts via
// the pure-cone selector length(payload.text) mod N — the data CHANGES
// between message 1 and message 2 (draft A → draft B), deterministically,
// no freeze.

const (
	dsTruthDS   = "truth"
	dsChatLeaf  = "__inputs.platform.twitch.g2nmathias.last_chat"
	dsDraftLeaf = "__vars..draft_display"
	dsChatDisp  = "chat.display"
	dsN         = 3 // DRAFT_N in the fixture
)

// The three PINNED candidate matches the on-start pre-fetch reads — the
// SAME real match_ids hard-coded in Blue's bp-draft-switch fixture
// (PINNED_MATCH_IDS), one per region. Each per-draft draft_picks query
// splices its pinned match_id into its where clause; there is no matches
// list query anymore (the draft-less-head prod trap is gone).
//   [0] LPL — Invictus Gaming vs Top Esports   (real first pick: Nautilus)
//   [1] LEC — GIANTX vs Fnatic                 (real first pick: Azir)
//   [2] LCK — Hanwha Life vs HANJIN BRION       (real first pick: Orianna)
var dsMatchIDs = []string{
	"4fb6bc23-51f6-4a70-9dbc-7ba51473976d",
	"1aa918cc-014e-41ca-bab3-1e7d313c66d7",
	"f7958e36-f646-4765-a8b1-034c1c52050e",
}

// Each draft's real first champion (blue side, pick_order ascending) —
// distinct per match so the displayed board visibly changes when the draft
// switches. champion is NOT NULL in ZabTruth; the display reads it directly.
// These mirror the real prod draft heads for the three pinned matches.
var dsFirstChamp = []string{"Nautilus", "Azir", "Orianna"}

// dsDraftResponse is the stub reply to a per-draft draft_picks query for a
// given match_id: a short, deterministic draft whose first pick champion is
// dsFirstChamp[k]. Robust columns only (side, champion, pick_order).
func dsDraftResponse(matchID string) string {
	k := -1
	for i, id := range dsMatchIDs {
		if id == matchID {
			k = i
		}
	}
	if k < 0 {
		return `{"rows":[],"count":0,"elapsed_ms":1}`
	}
	// two rows is enough to prove the switch (display head reads the rows it
	// has; missing later rows render null fields, never an error).
	champ := dsFirstChamp[k]
	return fmt.Sprintf(
		`{"rows":[`+
			`{"side":"blue","champion":%q,"pick_order":1},`+
			`{"side":"red","champion":%q,"pick_order":2}`+
			`],"count":2,"elapsed_ms":1}`,
		champ, champ+"-counter",
	)
}

// dsExpectedHeader is the first line of draft.display for a given 0-based
// draft index (the board header "Draft #<index+1>").
func dsExpectedHeader(idx int) string {
	return fmt.Sprintf("Draft #%d", idx+1)
}

// dsExpectedFirstPick is the first pick line for a given draft index:
// "<side> <champion>" (blue side, first pick).
func dsExpectedFirstPick(idx int) string {
	return fmt.Sprintf("blue %s", dsFirstChamp[idx])
}

func dsTruthStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/truth/api/v1/_query" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		var desc struct {
			Table string `json:"table"`
			Where []struct {
				Column string `json:"column"`
				Value  string `json:"value"`
			} `json:"where"`
		}
		_ = json.Unmarshal(buf, &desc)
		switch desc.Table {
		case "draft_picks":
			mid := ""
			if len(desc.Where) == 1 && desc.Where[0].Column == "match_id" {
				mid = desc.Where[0].Value
			}
			_, _ = w.Write([]byte(dsDraftResponse(mid)))
		case "matches":
			// No matches list query is authored anymore — the candidates are
			// pinned literals. If one ever arrives, the pinning regressed.
			t.Errorf("unexpected matches list query — candidates must be pinned")
			http.Error(w, "bad", http.StatusBadRequest)
		default:
			t.Errorf("unexpected table %q", desc.Table)
			http.Error(w, "bad", http.StatusBadRequest)
		}
	}))
}

func dsManifest() compiler.ComputeManifest {
	return compiler.ComputeManifest{
		"core.event.on-start@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.db.query@1":         {IsPure: false, IsBounded: false, Version: "1"},
		"core.variable.set@1":     {IsPure: true, IsBounded: true, Version: "1"},
		"core.input@1":            {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.get-field@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.set-field@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.list-append@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.data.list-at@1":     {IsPure: true, IsBounded: true, Version: "1"},
		"core.literal@1":          {IsPure: true, IsBounded: true, Version: "1"},
		"core.cast.to-string@1":   {IsPure: true, IsBounded: true, Version: "1"},
		"core.string.concat@1":    {IsPure: true, IsBounded: true, Version: "1"},
		"core.string.length@1":    {IsPure: true, IsBounded: true, Version: "1"},
		"core.math.mod@1":         {IsPure: true, IsBounded: true, Version: "1"},
		"core.math.add@1":         {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":           {IsPure: true, IsBounded: true, Version: "1"},
		"quasar.twitch.chat@1":    {IsPure: true, IsBounded: true, Version: "1"},
	}
}

// buildDraftSwitchScene compiles the AUTHORED graph (the prod artefact)
// through the real compiler + runtime, wired to the truth stub.
func buildDraftSwitchScene(t *testing.T, gwURL string) *Scene {
	t.Helper()
	raw, err := os.ReadFile("testdata/m4_draft_switch_graph.json")
	if err != nil {
		t.Fatalf("read draft-switch graph: %v", err)
	}
	var bp compiler.BlueprintGraph
	if err := json.Unmarshal(raw, &bp); err != nil {
		t.Fatalf("decode draft-switch graph: %v", err)
	}
	bp.ID = "bp-draft-switch"

	fetcher := &stubFetcher{
		layout:    &compiler.CanvasLayout{Version: "v1", Root: compiler.LayoutNode{Kind: "stack", ID: "root"}},
		blueprint: &bp,
		manifest:  dsManifest(),
	}
	graph, bundle, version, err := compiler.Compile(context.Background(), "m4-draft-switch",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-draft-switch"}, fetcher)
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
	sc := NewScene("m4-draft-switch", graph, bundle, NewComputeRegistry(), quietLogger())
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
			dsTruthDS: {Name: dsTruthDS, Svc: dsTruthDS},
		},
	})
	return sc
}

func dsFireStart(t *testing.T, sc *Scene) {
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

// dsChatEvent is the canonical Twitch chat envelope Quasar produces, with a
// controllable text (its length drives the selector).
func dsChatEvent(author, text string) json.RawMessage {
	ev := fmt.Sprintf(
		`{"platform":"twitch","channel":"g2nmathias","type":"chat",`+
			`"ts":"2026-06-12T12:00:00Z",`+
			`"actor":{"platform_user_id":"1","display_name":%q,`+
			`"is_subscriber":false,"is_moderator":false,"badges":[]},`+
			`"payload":{"text":%q,"emotes":[],"reply_to":null}}`,
		author, text,
	)
	return json.RawMessage(ev)
}

// TestDraftSwitch_ChatChangesDraft is THE franchissement proof: two chat
// messages of different length select two DIFFERENT pre-fetched drafts; the
// draft board CHANGES between them (A → B) and the chat line reflects each.
func TestDraftSwitch_ChatChangesDraft(t *testing.T) {
	gw := dsTruthStub(t)
	defer gw.Close()

	sc := buildDraftSwitchScene(t, gw.URL)
	startScene(t, sc)

	// on-start pre-fetches the three drafts onto their state leaves. Wait
	// until all three have landed (draft_2 is the spine terminus).
	dsFireStart(t, sc)
	dsWaitNonNull(t, sc, "__vars..draft_0", 5*time.Second)
	dsWaitNonNull(t, sc, "__vars..draft_2", 5*time.Second)

	// --- message 1: a text whose length mod N selects draft A ---
	textA, idxA := dsTextForIndex("first message") // len 13 → 13%3 = 1
	sc.Input(InputMsg{Path: dsChatLeaf, Value: dsChatEvent("alice", textA), Source: "quasar:twitch"})

	// chat.display reflects message 1 (M1 reactive guard).
	waitForState(t, sc, dsChatDisp, fmt.Sprintf("%q", "alice: "+textA), 5*time.Second)
	// draft.display shows draft A (header + first pick of the selected draft).
	dsWaitDisplayShows(t, sc, idxA, 5*time.Second)

	// --- message 2: a DIFFERENT length → a DIFFERENT draft (the SWITCH) ---
	textB, idxB := dsTextForIndex("yo") // len 2 → 2%3 = 2 ≠ idxA
	if idxB == idxA {
		t.Fatalf("test setup: message lengths must select different drafts (idxA=idxB=%d)", idxA)
	}
	sc.Input(InputMsg{Path: dsChatLeaf, Value: dsChatEvent("bob", textB), Source: "quasar:twitch"})

	waitForState(t, sc, dsChatDisp, fmt.Sprintf("%q", "bob: "+textB), 5*time.Second)
	// draft.display now shows draft B — the data CHANGED between the two
	// messages (proof of dynamism, no freeze).
	dsWaitDisplayShows(t, sc, idxB, 5*time.Second)
}

// dsTextForIndex returns a text and the draft index its length selects
// (length mod N) — the index is DERIVED from the text, so the assertion is
// honest about what the selector actually computes.
func dsTextForIndex(text string) (string, int) {
	return text, utf8.RuneCountInString(text) % dsN
}

// dsWaitNonNull waits until a state leaf is present and not JSON null —
// proof the exec pre-fetch landed a draft.
func dsWaitNonNull(t *testing.T, sc *Scene, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get(path); ok && string(v) != "null" && len(v) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, _ := sc.state.Get(path)
	t.Fatalf("leaf %s never landed a draft; last = %q", path, v)
}

// dsWaitDisplayShows waits until draft.display carries the header AND the
// first-pick line of the given draft index — i.e. the selected draft is the
// one on the antenna.
func dsWaitDisplayShows(t *testing.T, sc *Scene, idx int, timeout time.Duration) {
	t.Helper()
	wantHeader := dsExpectedHeader(idx)
	wantPick := dsExpectedFirstPick(idx)
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if v, ok := sc.state.Get(dsDraftLeaf); ok {
			var s string
			if json.Unmarshal(v, &s) == nil {
				last = s
				if strings.Contains(s, wantHeader) && strings.Contains(s, wantPick) {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("draft.display never showed draft #%d (want header %q + pick %q); last = %q",
		idx+1, wantHeader, wantPick, last)
}
