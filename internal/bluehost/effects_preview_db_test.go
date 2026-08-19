package bluehost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

func TestEffectHandlers_PreviewExecutesReadOnlyDBQuery(t *testing.T) {
	var gotPath, gotAuth string
	var gotDescriptor map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotDescriptor); err != nil {
			t.Fatalf("decode query descriptor: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rows":[{"match_id":"lec-2026-01","league":"LEC","home":"Natus Vincere","away":"Team Heretics"}],"count":1,"elapsed_ms":3}`))
	}))
	defer server.Close()

	var gotScopes []string
	db := effects.NewDBQueryClientWithPathTokenFunc(server.URL, func(scopes []string) string {
		gotScopes = append([]string(nil), scopes...)
		return "preview-query-token"
	}, nil)
	handler := NewEffectHandlers(EffectDeps{
		DB: db,
		DataSources: map[string]effects.DataSource{
			"truth": {Name: "truth", Svc: "truth"},
		},
	}, blueruntime.Preview)["core.db.query@1"]

	outputs, err := handler(
		map[string]any{"datasource": "truth"},
		map[string]any{"descriptor": map[string]any{
			"table":  "matches",
			"select": []any{"match_id", "league", "home", "away"},
			"where":  map[string]any{"league": "LEC"},
		}},
	)
	if err != nil {
		t.Fatalf("preview db.query: %v", err)
	}
	if gotPath != "/truth/api/v1/_query" {
		t.Fatalf("query path = %q", gotPath)
	}
	if gotAuth != "Bearer preview-query-token" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if !reflect.DeepEqual(gotScopes, []string{"query.read.truth"}) {
		t.Fatalf("token scopes = %#v", gotScopes)
	}
	if !reflect.DeepEqual(gotDescriptor, map[string]any{
		"table":  "matches",
		"select": []any{"match_id", "league", "home", "away"},
		"where":  map[string]any{"league": "LEC"},
	}) {
		t.Fatalf("descriptor = %#v", gotDescriptor)
	}
	if count, ok := outputs["count"].(json.Number); !ok || count.String() != "1" {
		t.Fatalf("count = %#v", outputs["count"])
	}
	rows, ok := outputs["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("rows = %#v", outputs["rows"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok || row["league"] != "LEC" || row["home"] != "Natus Vincere" || row["away"] != "Team Heretics" {
		t.Fatalf("match row = %#v", rows[0])
	}
}
