package datasidecar

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer spins the sidecar over httptest, exercising the SAME path
// Orion's DBQueryClient writes: POST /<svc>/api/v1/_query.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s, err := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return ts
}

type queryResp struct {
	Rows      []map[string]any `json:"rows"`
	Count     int              `json:"count"`
	ElapsedMS json.Number      `json:"elapsed_ms"`
}

// doQueryRaw posts a descriptor and returns the status + full body bytes,
// always closing the response body.
func doQueryRaw(t *testing.T, base, svc, descriptor string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(base+"/"+svc+"/api/v1/_query", "application/json",
		strings.NewReader(descriptor))
	if err != nil {
		t.Fatalf("POST _query: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

func doQuery(t *testing.T, base, svc, descriptor string) (int, queryResp) {
	t.Helper()
	status, body := doQueryRaw(t, base, svc, descriptor)
	var qr queryResp
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &qr); err != nil {
			t.Fatalf("decode 200: %v", err)
		}
	}
	return status, qr
}

// --- The three scene queries (match-by-league / match-roster / score-for-player) ---

// TestScene_MatchByLeague: the scene resolves a league's latest match.
func TestScene_MatchByLeague(t *testing.T) {
	ts := newTestServer(t)
	_, qr := doQuery(t, ts.URL, "truth", `{
		"table":"matches",
		"where":[{"column":"league","op":"=","value":"LEC"}],
		"select":["id","league","blue_team","red_team"],
		"order":[{"column":"played_at","direction":"desc"}],
		"limit":1
	}`)
	if qr.Count != 1 {
		t.Fatalf("count = %d, want 1", qr.Count)
	}
	r := qr.Rows[0]
	if r["id"] != lecMatchID {
		t.Fatalf("id = %v, want frozen LEC match id %s", r["id"], lecMatchID)
	}
	// uuid must be a lowercase-hex string (parity trap #1).
	if s, ok := r["id"].(string); !ok || s != strings.ToLower(s) {
		t.Fatalf("uuid must be lowercase-hex string, got %#v", r["id"])
	}
	if r["blue_team"] != "Movistar KOI" || r["red_team"] != "G2 Esports" {
		t.Fatalf("teams = %v / %v", r["blue_team"], r["red_team"])
	}
}

// TestScene_MatchRoster: match-roster by match_id returns 10 players.
func TestScene_MatchRoster(t *testing.T) {
	ts := newTestServer(t)
	_, qr := doQuery(t, ts.URL, "truth", `{
		"table":"match_players",
		"joins":[{"table":"players","on":["player_id","id"],"select":["summoner_name"]}],
		"where":[{"column":"match_players.match_id","op":"=","value":"`+lckMatchID+`"}],
		"select":["side","role","champion","win"]
	}`)
	if qr.Count != 10 {
		t.Fatalf("roster count = %d, want 10", qr.Count)
	}
	// win is boolean → JSON true/false (not 0/1 string).
	for _, row := range qr.Rows {
		if _, ok := row["win"].(bool); !ok {
			t.Fatalf("win must be a JSON bool, got %#v", row["win"])
		}
	}
}

// TestScene_ScoreForPlayer: score-for-player by player_id returns the score
// as a JSON float (parity trap #3: Numeric(4,2) → float, never string).
func TestScene_ScoreForPlayer(t *testing.T) {
	ts := newTestServer(t)
	playerID := detUUID("Zeus") // first LCK player → cycle[0] = 9.5
	_, qr := doQuery(t, ts.URL, "ranking", `{
		"table":"player_scores",
		"where":[
			{"column":"player_id","op":"=","value":"`+playerID+`"},
			{"column":"match_id","op":"=","value":"`+lckMatchID+`"}
		],
		"select":["score","comment"]
	}`)
	if qr.Count != 1 {
		t.Fatalf("count = %d, want 1", qr.Count)
	}
	score := qr.Rows[0]["score"]
	f, ok := score.(float64)
	if !ok {
		t.Fatalf("score must be a JSON float, got %#v (%T)", score, score)
	}
	if f != 9.5 {
		t.Fatalf("score = %v, want 9.5 (cycle[0])", f)
	}
}

// --- The four mandatory parity traps (contract §A.6 #1-4) ---

// Trap #1 — scalar coercion: uuid lowercase-hex, datetime ISO-8601, null
// passthrough.
func TestParity_ScalarCoercion(t *testing.T) {
	ts := newTestServer(t)
	_, qr := doQuery(t, ts.URL, "truth", `{
		"table":"matches",
		"where":[{"column":"id","op":"=","value":"`+lckMatchID+`"}],
		"select":["id","played_at","notes"]
	}`)
	if qr.Count != 1 {
		t.Fatalf("count = %d", qr.Count)
	}
	r := qr.Rows[0]
	if r["id"] != lckMatchID {
		t.Fatalf("uuid = %#v", r["id"])
	}
	if r["played_at"] != seedTime {
		t.Fatalf("datetime = %#v, want ISO-8601 %s", r["played_at"], seedTime)
	}
	if v, ok := r["notes"]; !ok || v != nil {
		t.Fatalf("NULL column must passthrough as JSON null, got %#v", r["notes"])
	}
}

// Trap #2 — LIKE is case-SENSITIVE (Postgres LIKE; queryme emits no ILIKE).
func TestParity_LikeCaseSensitive(t *testing.T) {
	ts := newTestServer(t)
	// Exact-case prefix matches.
	_, hit := doQuery(t, ts.URL, "truth", `{
		"table":"players",
		"where":[{"column":"summoner_name","op":"LIKE","value":"Caps%"}],
		"select":["summoner_name"]
	}`)
	if hit.Count != 1 {
		t.Fatalf("case-matching LIKE count = %d, want 1", hit.Count)
	}
	// Wrong case must NOT match (would match if SQLite stayed case-insensitive).
	_, miss := doQuery(t, ts.URL, "truth", `{
		"table":"players",
		"where":[{"column":"summoner_name","op":"LIKE","value":"caps%"}],
		"select":["summoner_name"]
	}`)
	if miss.Count != 0 {
		t.Fatalf("case-mismatching LIKE count = %d, want 0 (LIKE must be case-sensitive)", miss.Count)
	}
}

// Trap #3 — covered by TestScene_ScoreForPlayer (Numeric → float).

// Trap #4 — Postgres NULL ordering: NULLs LAST on asc, FIRST on desc, over a
// nullable column. We seed one extra match with a NULL played_at to exercise it.
func TestParity_NullOrdering(t *testing.T) {
	s, err := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Inject an LCK match with NULL played_at directly into the mirror.
	nullMatch := "00000000-0000-4000-8000-0000000000ff"
	if err := insertRow(t.Context(), s.dbs["truth"], "matches", map[string]any{
		"id": nullMatch, "league": "LCK", "blue_team": "NULLTEAM", "red_team": "x",
		"created_at": seedTime, "updated_at": seedTime, // played_at omitted → NULL
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// asc → NULL played_at must sort LAST (Postgres default).
	_, asc := doQuery(t, ts.URL, "truth", `{
		"table":"matches",
		"where":[{"column":"league","op":"=","value":"LCK"}],
		"select":["id","played_at"],
		"order":[{"column":"played_at","direction":"asc"}]
	}`)
	last := asc.Rows[len(asc.Rows)-1]
	if last["played_at"] != nil {
		t.Fatalf("asc: NULL played_at must sort LAST, last row = %#v", last)
	}
	// desc → NULL played_at must sort FIRST.
	_, desc := doQuery(t, ts.URL, "truth", `{
		"table":"matches",
		"where":[{"column":"league","op":"=","value":"LCK"}],
		"select":["id","played_at"],
		"order":[{"column":"played_at","direction":"desc"}]
	}`)
	if desc.Rows[0]["played_at"] != nil {
		t.Fatalf("desc: NULL played_at must sort FIRST, first row = %#v", desc.Rows[0])
	}
}

// --- Envelope + error parity ---

// Test200Envelope: count is an int, elapsed_ms is a JSON number (decodes into
// Orion's float64), rows is a list of {col:scalar}.
func Test200Envelope(t *testing.T) {
	ts := newTestServer(t)
	status, qr := doQuery(t, ts.URL, "ranking", `{"table":"splits","select":["id","name","is_active"]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if qr.Count != 1 {
		t.Fatalf("count = %d", qr.Count)
	}
	if _, err := qr.ElapsedMS.Float64(); err != nil {
		t.Fatalf("elapsed_ms not a number: %v", err)
	}
	if b, ok := qr.Rows[0]["is_active"].(bool); !ok || !b {
		t.Fatalf("is_active must be JSON bool true, got %#v", qr.Rows[0]["is_active"])
	}
}

// Test400CarriesIssues: an unknown table yields 400 with the
// {"detail":{"issues":[...]}} envelope (the substring "issues" Orion's
// TestDBQuery_400CarriesIssues asserts).
func Test400CarriesIssues(t *testing.T) {
	ts := newTestServer(t)
	status, body := doQueryRaw(t, ts.URL, "truth", `{"table":"nope","select":["x"]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	var env struct {
		Detail struct {
			Issues []validationIssue `json:"issues"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode 400: %v (body=%s)", err, body)
	}
	if len(env.Detail.Issues) == 0 || env.Detail.Issues[0].Code != "unknown_table" {
		t.Fatalf("issues = %#v, want one unknown_table", env.Detail.Issues)
	}
	if !strings.Contains(string(body), "issues") {
		t.Fatalf("400 body must carry 'issues': %s", body)
	}
}

// TestExtraFieldRejected: extra="forbid" parity — unknown descriptor keys 400.
func TestExtraFieldRejected(t *testing.T) {
	ts := newTestServer(t)
	status, _ := doQueryRaw(t, ts.URL, "truth", `{"table":"players","select":["id"],"bogus":1}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field", status)
	}
}

// TestJoinKeyCollision: a join that re-projects a FROM column name qualifies
// the joined duplicate as "<join.table>.<col>" (row-key derivation parity).
func TestJoinKeyCollision(t *testing.T) {
	ts := newTestServer(t)
	_, qr := doQuery(t, ts.URL, "truth", `{
		"table":"match_players",
		"joins":[{"table":"players","on":["player_id","id"],"select":["id","summoner_name"]}],
		"where":[{"column":"match_players.match_id","op":"=","value":"`+lckMatchID+`"}],
		"select":["id","champion"]
	}`)
	if qr.Count != 10 {
		t.Fatalf("count = %d, want 10", qr.Count)
	}
	// "id" appears on both match_players (FROM) and players (join) → the
	// joined one is qualified "players.id"; FROM keeps bare "id".
	r := qr.Rows[0]
	if _, ok := r["id"]; !ok {
		t.Fatalf("FROM id must keep bare key, row = %#v", r)
	}
	if _, ok := r["players.id"]; !ok {
		t.Fatalf("joined duplicate must be qualified players.id, row = %#v", r)
	}
}
