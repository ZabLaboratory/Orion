package effects

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServiceTokenExchangeMinterCachesByExactScope(t *testing.T) {
	var calls int
	var auth string
	var requests []exchangeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		auth = r.Header.Get("Authorization")
		var req exchangeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"delegated","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	minter := NewServiceTokenExchangeMinter(srv.URL, "family-token", srv.Client())
	if got := minter.Token([]string{"query.read.truth"}); got != "delegated" {
		t.Fatalf("first token = %q", got)
	}
	if got := minter.Token([]string{"query.read.truth"}); got != "delegated" {
		t.Fatalf("cached token = %q", got)
	}
	if got := minter.Token([]string{"query.read.ranking"}); got != "delegated" {
		t.Fatalf("second scope token = %q", got)
	}
	if calls != 2 {
		t.Fatalf("exchange calls = %d, want 2 (one per exact scope)", calls)
	}
	if auth != "Bearer family-token" {
		t.Fatalf("source authorization = %q", auth)
	}
	if len(requests) != 2 || len(requests[0].Paths) != 1 || requests[0].Paths[0] != "query.read.truth" || requests[0].TTLS != 300 {
		t.Fatalf("unexpected exchange requests: %+v", requests)
	}
}

func TestServiceTokenExchangeMinterFailsClosedWithoutFamily(t *testing.T) {
	minter := NewServiceTokenExchangeMinter("http://127.0.0.1:1", "", nil)
	if got := minter.Token([]string{"query.read.truth"}); got != "" {
		t.Fatalf("token = %q, want empty", got)
	}
}
