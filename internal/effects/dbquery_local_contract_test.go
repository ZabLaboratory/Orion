package effects

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Contract lock for the embedded-local data sidecar (ADR 016 §B, issue
// #225). The local SQLite sidecar must answer POST /<svc>/api/v1/_query
// with the SAME 200 envelope ZabGate→ZabTruth/ZabRanking serves today,
// so the db.query hot path stays BIT-IDENTICAL between antenna and local.
// This test decodes that frozen envelope through Orion's real QueryResult
// consumer (dbquery.go) — it is the executable target #225 builds to.
// See docs/contracts/embedded-local-contracts.md §A.

// TestLocalQueryContract_200Envelope locks the exact 200 shape:
// {"rows":[{col:scalar}],"count":int,"elapsed_ms":number}. The sidecar's
// rows[] are opaque to Orion (json.RawMessage) but count/elapsed_ms are
// decoded; elapsed_ms is float64 on Orion's side and an int on the
// services' side — both must round-trip (int decodes into float64).
func TestLocalQueryContract_200Envelope(t *testing.T) {
	// Verbatim shape a parity sidecar must emit, incl. the _serialise
	// coercions (uuid→str, decimal→float, datetime→iso, null passthrough).
	const body = `{"rows":[` +
		`{"id":"3f2504e0-4f89-41d3-9a0c-0305e82c3301","summoner_name":"zab","score":7.5,"played_at":"2026-06-21T10:00:00+00:00","note":null}` +
		`],"count":1,"elapsed_ms":12}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewDBQueryClient(srv.URL, "tok", nil)
	res, err := c.Query(context.Background(), DataSource{Name: "ranking", Svc: "ranking"},
		json.RawMessage(`{"table":"player_scores","select":["score"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 1 {
		t.Fatalf("count = %d, want 1", res.Count)
	}
	if res.ElapsedMS != 12 {
		t.Fatalf("elapsed_ms = %v, want 12 (int must decode into float64)", res.ElapsedMS)
	}
	// rows is opaque but must be valid JSON the runtime can re-decode, and
	// must carry the coerced scalars verbatim.
	var rows []map[string]any
	if err := json.Unmarshal(res.Rows, &rows); err != nil {
		t.Fatalf("rows not decodable: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	r := rows[0]
	if r["id"] != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("uuid must be a lowercase-hex string, got %#v", r["id"])
	}
	if r["score"] != 7.5 {
		t.Fatalf("Numeric must be a JSON float, got %#v", r["score"])
	}
	if _, ok := r["note"]; !ok || r["note"] != nil {
		t.Fatalf("NULL column must passthrough as JSON null, got %#v", r["note"])
	}
}

// TestLocalQueryContract_400Issues locks the error envelope a sidecar must
// keep on a bad descriptor: a non-200 whose body carries the structured
// issues, surfaced verbatim to the effect's error port (no crash).
func TestLocalQueryContract_400Issues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":{"issues":[{"table":"nope","reason":"unknown table"}]}}`))
	}))
	defer srv.Close()

	c := NewDBQueryClient(srv.URL, "tok", nil)
	_, err := c.Query(context.Background(), DataSource{Name: "ranking", Svc: "ranking"},
		json.RawMessage(`{"table":"nope","select":[]}`))
	if err == nil || !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "issues") {
		t.Fatalf("err = %v, want a 400 carrying the issues envelope", err)
	}
}
