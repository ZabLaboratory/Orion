package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEgressTokenSource_StaticModeFailsClosed(t *testing.T) {
	// No operator token → no egress (fail-closed), never an anonymous mint.
	s := &EgressTokenSource{ServiceName: "orion"}
	if tok := s.Token([]string{"example.echo"}); tok != "" {
		t.Errorf("static mode returned a token %q, want \"\" (fail-closed)", tok)
	}
}

func TestEgressTokenSource_EmptyPathsFailsClosed(t *testing.T) {
	s := &EgressTokenSource{OperatorToken: "op", ServiceName: "orion"}
	if tok := s.Token(nil); tok != "" {
		t.Errorf("empty paths returned %q, want \"\"", tok)
	}
}

func TestEgressTokenSource_MintsWithExactPaths(t *testing.T) {
	var gotPaths []string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Service string   `json:"service"`
			Paths   []string `json:"paths"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotPaths = body.Paths
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ServiceTokenBundle{
			AccessToken: "minted-token",
			ExpiresAt:   time.Now().Add(time.Hour),
		})
	}))
	defer srv.Close()

	s := &EgressTokenSource{
		MintURL:       srv.URL,
		OperatorToken: "op-jwt",
		ServiceName:   "orion",
	}
	tok := s.Token([]string{"example.echo"})
	if tok != "minted-token" {
		t.Fatalf("token = %q, want minted-token", tok)
	}
	if len(gotPaths) != 1 || gotPaths[0] != "example.echo" {
		t.Errorf("minted paths = %v, want [example.echo]", gotPaths)
	}
	if gotAuth != "Bearer op-jwt" {
		t.Errorf("mint auth = %q, want the operator bearer", gotAuth)
	}

	// Second call for the same scope is served from cache (no re-mint while
	// the bundle is valid).
	if tok2 := s.Token([]string{"example.echo"}); tok2 != "minted-token" {
		t.Errorf("cached token = %q", tok2)
	}
}
