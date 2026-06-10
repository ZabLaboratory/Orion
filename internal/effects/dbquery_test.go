package effects

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the db.query topology-A client (ADR 003 Amendment 1, wire
// contract §1): _query delegation via ZabGate, service token attached,
// no DB credential anywhere on this side of the wire.

// TestParseDataSources_CSV pins the étage-1 ORION_DATASOURCES wire
// form: `<logical_name>=<zabgate_svc>` CSV, per-service scope derived.
func TestParseDataSources_CSV(t *testing.T) {
	ds, err := ParseDataSources("truth=truth, ranking=ranking")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds["truth"].Svc != "truth" || ds["ranking"].Svc != "ranking" {
		t.Fatalf("parsed = %+v", ds)
	}
	if got := ds["truth"].Scope(); got != "query.read.truth" {
		t.Fatalf("scope = %q, want query.read.truth", got)
	}
	if m, err := ParseDataSources(""); err != nil || len(m) != 0 {
		t.Fatalf("empty input must parse to the empty allowlist, got %v / %v", m, err)
	}
	if _, err := ParseDataSources("truthtruth"); err == nil {
		t.Fatal("malformed entry must fail")
	}
	if _, err := ParseDataSources("a=x,a=y"); err == nil {
		t.Fatal("duplicate name must fail")
	}
}

// TestDBQuery_WireShape proves the exact request Orion emits: POST
// `${gateway}/<svc>/api/v1/_query`, Bearer service token, descriptor
// body passed through verbatim — and the {rows,count,elapsed_ms}
// response decoded.
func TestDBQuery_WireShape(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rows":[{"summoner_name":"zab"}],"count":1,"elapsed_ms":12}`))
	}))
	defer srv.Close()

	c := NewDBQueryClient(srv.URL+"/", "tok-123", nil)
	desc := json.RawMessage(`{"table":"players","select":["summoner_name"]}`)
	res, err := c.Query(context.Background(), DataSource{Name: "truth", Svc: "truth"}, desc)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/truth/api/v1/_query" {
		t.Fatalf("wire = %s %s, want POST /truth/api/v1/_query", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" || gotBody != string(desc) {
		t.Fatalf("body wire = %q %q", gotCT, gotBody)
	}
	if res.Count != 1 || !strings.Contains(string(res.Rows), "zab") || res.ElapsedMS != 12 {
		t.Fatalf("decoded = %+v", res)
	}
}

// TestDBQuery_400CarriesIssues: an invalid descriptor's 400 surfaces
// the service's structured issues in the error (→ the effect's error
// port), never a crash.
func TestDBQuery_400CarriesIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"issues":[{"column":"nope"}]}}`))
	}))
	defer srv.Close()

	c := NewDBQueryClient(srv.URL, "tok", nil)
	_, err := c.Query(context.Background(), DataSource{Name: "truth", Svc: "truth"}, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "issues") {
		t.Fatalf("err = %v", err)
	}
}
