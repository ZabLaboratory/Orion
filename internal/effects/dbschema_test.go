package effects

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// Tests for the read-only DB schema client (ADR Blue 008 §3.4). Orion
// proxies the owning service's `_schema` — the same catalog its query
// validator whitelists against — and never executes a query here.

const truthSchemaJSON = `{
  "service": "truth",
  "tables": [
    {"name": "players", "columns": [
      {"name": "id", "type": "uuid", "primary": true},
      {"name": "summoner_name", "type": "string"},
      {"name": "notes", "type": "text", "nullable": true},
      {"name": "created_at", "type": "datetime"}
    ]}
  ]
}`

// TestSchema_WireShape pins the exact request: GET
// `${gateway}/<svc>/api/v1/_schema`, Bearer service token, no body —
// and the catalog decoded.
func TestSchema_WireShape(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(truthSchemaJSON))
	}))
	defer srv.Close()

	c := NewSchemaClientWithTokenFunc(srv.URL+"/", func() string { return "tok-123" }, nil)
	schema, err := c.Schema(context.Background(), DataSource{Name: "truth", Svc: "truth"})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/truth/api/v1/_schema" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if schema.Service != "truth" || len(schema.Tables) != 1 || schema.Tables[0].Name != "players" {
		t.Fatalf("schema = %+v", schema)
	}
}

// TestSchema_Non200IsError proves a non-200 from the owning service
// surfaces as an error (never a silent empty catalog).
func TestSchema_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"nope"}`))
	}))
	defer srv.Close()

	c := NewSchemaClientWithTokenFunc(srv.URL, func() string { return "" }, nil)
	if _, err := c.Schema(context.Background(), DataSource{Name: "truth", Svc: "truth"}); err == nil {
		t.Fatal("non-200 must be an error")
	}
}

// TestSearchableColumns_NonPrimaryStringText pins the derivation:
// searchable = non-primary string/text columns only (no UUID PK, no
// temporal/numeric), declaration order preserved.
func TestSearchableColumns_NonPrimaryStringText(t *testing.T) {
	tbl := SchemaTable{Columns: []SchemaColumn{
		{Name: "id", Type: "uuid", Primary: true},
		{Name: "summoner_name", Type: "string"},
		{Name: "notes", Type: "text", Nullable: true},
		{Name: "created_at", Type: "datetime"},
	}}
	got := tbl.SearchableColumns()
	want := []string{"summoner_name", "notes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("searchable = %v, want %v", got, want)
	}
}

// TestResultShape_AllColumns pins the result shape: every whitelisted
// column name → type, including the PK and non-searchable columns.
func TestResultShape_AllColumns(t *testing.T) {
	tbl := SchemaTable{Columns: []SchemaColumn{
		{Name: "id", Type: "uuid", Primary: true},
		{Name: "summoner_name", Type: "string"},
	}}
	got := tbl.ResultShape()
	want := map[string]string{"id": "uuid", "summoner_name": "string"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result_shape = %v, want %v", got, want)
	}
}
