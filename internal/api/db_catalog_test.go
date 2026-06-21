package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// DB catalog surface (ADR Blue 008 §3.4, issue #211, RC-5): both
// surfaces return a searchable schema for `truth` and `ranking`,
// derived strictly from each service's read-only `_schema`.

func catalogStubGateway(t *testing.T) *httptest.Server {
	t.Helper()
	bodies := map[string]string{
		"/truth/api/v1/_schema": `{"service":"truth","tables":[
			{"name":"players","columns":[
				{"name":"id","type":"uuid","primary":true},
				{"name":"summoner_name","type":"string"},
				{"name":"created_at","type":"datetime"}]},
			{"name":"matches","columns":[
				{"name":"id","type":"uuid","primary":true},
				{"name":"league","type":"string"}]}]}`,
		"/ranking/api/v1/_schema": `{"service":"ranking","tables":[
			{"name":"player_scores","columns":[
				{"name":"id","type":"uuid","primary":true},
				{"name":"player_name","type":"string"},
				{"name":"score","type":"float"}]}]}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func catalogDeps(gateway string) PublicDeps {
	return PublicDeps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: config.Config{DataSources: map[string]string{
			"truth":   "truth",
			"ranking": "ranking",
		}},
		SchemaClient: effects.NewSchemaClientWithTokenFunc(gateway, func() string { return "tok" }, nil),
	}
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(w.Body.String()), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	return out
}

// TestDBSchema_TruthAndRanking — GET /db/{service}/schema returns the
// table list with searchable_columns + result_shape for both services.
func TestDBSchema_TruthAndRanking(t *testing.T) {
	srv := catalogStubGateway(t)
	defer srv.Close()
	deps := catalogDeps(srv.URL)

	for _, svc := range []string{"truth", "ranking"} {
		r := httptest.NewRequest("GET", "/api/v1/db/"+svc+"/schema", nil)
		r.SetPathValue("service", svc)
		w := httptest.NewRecorder()
		getDBSchema(deps)(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body=%s", svc, w.Code, w.Body.String())
		}
		body := decodeBody(t, w)
		if body["service"] != svc {
			t.Fatalf("%s: service field = %v", svc, body["service"])
		}
		tables, ok := body["tables"].([]any)
		if !ok || len(tables) == 0 {
			t.Fatalf("%s: no tables: %v", svc, body["tables"])
		}
		first := tables[0].(map[string]any)
		if _, ok := first["searchable_columns"]; !ok {
			t.Fatalf("%s: table missing searchable_columns", svc)
		}
		if _, ok := first["result_shape"]; !ok {
			t.Fatalf("%s: table missing result_shape", svc)
		}
	}
}

// TestDBSchema_SearchableExcludesPKAndNonText — players.searchable is
// summoner_name only (PK uuid + datetime excluded); result_shape carries
// every column.
func TestDBSchema_SearchableExcludesPKAndNonText(t *testing.T) {
	srv := catalogStubGateway(t)
	defer srv.Close()
	deps := catalogDeps(srv.URL)

	r := httptest.NewRequest("GET", "/api/v1/db/truth/schema", nil)
	r.SetPathValue("service", "truth")
	w := httptest.NewRecorder()
	getDBSchema(deps)(w, r)

	body := decodeBody(t, w)
	players := body["tables"].([]any)[0].(map[string]any)
	sc := players["searchable_columns"].([]any)
	if len(sc) != 1 || sc[0] != "summoner_name" {
		t.Fatalf("searchable = %v, want [summoner_name]", sc)
	}
	shape := players["result_shape"].(map[string]any)
	if shape["id"] != "uuid" || shape["created_at"] != "datetime" {
		t.Fatalf("result_shape = %v", shape)
	}
}

// TestDBSchema_UndeclaredDatasource — a name outside ORION_DATASOURCES
// is refused with DATASOURCE_NOT_DECLARED (no wider lookup than the
// allowlist).
func TestDBSchema_UndeclaredDatasource(t *testing.T) {
	srv := catalogStubGateway(t)
	defer srv.Close()
	deps := catalogDeps(srv.URL)

	r := httptest.NewRequest("GET", "/api/v1/db/secret/schema", nil)
	r.SetPathValue("service", "secret")
	w := httptest.NewRecorder()
	getDBSchema(deps)(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if decodeBody(t, w)["code"] != "DATASOURCE_NOT_DECLARED" {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// TestListDatasources_DBTableEntries — GET /db/datasources emits one
// kind=db.table entry per (datasource, table), sorted by datasource,
// each carrying service/table/searchable_columns/result_shape.
func TestListDatasources_DBTableEntries(t *testing.T) {
	srv := catalogStubGateway(t)
	defer srv.Close()
	deps := catalogDeps(srv.URL)

	r := httptest.NewRequest("GET", "/api/v1/db/datasources", nil)
	w := httptest.NewRecorder()
	listDatasources(deps)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	entries := decodeBody(t, w)["datasources"].([]any)
	// truth(players, matches) + ranking(player_scores) = 3 entries.
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %s", len(entries), w.Body.String())
	}
	seen := map[string]bool{}
	for _, e := range entries {
		m := e.(map[string]any)
		if m["kind"] != "db.table" {
			t.Fatalf("kind = %v, want db.table", m["kind"])
		}
		for _, k := range []string{"service", "table", "searchable_columns", "result_shape"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("entry missing %q: %v", k, m)
			}
		}
		seen[m["service"].(string)+"."+m["table"].(string)] = true
	}
	for _, want := range []string{"truth.players", "truth.matches", "ranking.player_scores"} {
		if !seen[want] {
			t.Fatalf("missing entry %s; seen=%v", want, seen)
		}
	}
}

// TestListDatasources_SkipsUnreachable — a datasource whose _schema is
// down is skipped (graceful degradation), the rest still listed.
func TestListDatasources_SkipsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/truth/api/v1/_schema" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"service":"truth","tables":[{"name":"players","columns":[{"name":"summoner_name","type":"string"}]}]}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	deps := catalogDeps(srv.URL)

	r := httptest.NewRequest("GET", "/api/v1/db/datasources", nil)
	w := httptest.NewRecorder()
	listDatasources(deps)(w, r)
	entries := decodeBody(t, w)["datasources"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (ranking skipped): %s", len(entries), w.Body.String())
	}
}
