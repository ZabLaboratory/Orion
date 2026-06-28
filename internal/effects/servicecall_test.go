package effects

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildPath_FillsAndEscapes(t *testing.T) {
	tmpl := "/example/api/v1/items/{name}/echo"
	p, err := BuildPath(tmpl, []string{"name"}, map[string]string{"name": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if p != "/example/api/v1/items/alice/echo" {
		t.Errorf("path = %q", p)
	}
}

func TestBuildPath_EscapesInjection(t *testing.T) {
	tmpl := "/x/{name}/y"
	p, err := BuildPath(tmpl, []string{"name"}, map[string]string{"name": "../../a?b=1&c=2"})
	if err != nil {
		t.Fatal(err)
	}
	// Every reserved char escaped (unreserved-only, parity with Blue):
	// no raw slash, no '?', '&', '=', so the value stays one segment.
	want := "/x/..%2F..%2Fa%3Fb%3D1%26c%3D2/y"
	if p != want {
		t.Errorf("escaped = %q, want %q", p, want)
	}
}

func TestBuildPath_MissingParam(t *testing.T) {
	if _, err := BuildPath("/x/{a}", []string{"a"}, map[string]string{}); err == nil {
		t.Fatal("expected error for missing param")
	}
}

func TestServiceCall_NoTokenFailsClosed(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()
	// mint returns "" → no scoped token → fail-closed, no request emitted.
	c := NewServiceCallClient(srv.URL, func([]string) string { return "" }, nil)
	_, err := c.Call(context.Background(), "POST", "/x", []string{"p"}, nil)
	if err == nil {
		t.Fatal("expected fail-closed error when no scoped token")
	}
	if called {
		t.Error("server hit despite no token — must not emit anonymous request")
	}
}

func TestServiceCall_Non2xxSurfacesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"nope"}`))
	}))
	defer srv.Close()
	c := NewServiceCallClient(srv.URL, func([]string) string { return "tok" }, nil)
	res, err := c.Call(context.Background(), "POST", "/x", []string{"p"}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("4xx is a result, not a transport error: %v", err)
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.Status)
	}
	if string(res.Body) != `{"detail":"nope"}` {
		t.Errorf("body = %s", res.Body)
	}
}

func TestServiceCall_DoesNotForwardCallerAuth(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	// The client only ever sets ITS OWN scoped bearer — there is no API
	// surface to inject a caller Authorization, which is the §3.3 guarantee.
	c := NewServiceCallClient(srv.URL, func([]string) string { return "scoped" }, nil)
	if _, err := c.Call(context.Background(), "POST", "/x", []string{"p"}, nil); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer scoped" {
		t.Errorf("authorization = %q, want the scoped bearer only", got)
	}
}
