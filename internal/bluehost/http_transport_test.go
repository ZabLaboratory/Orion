package bluehost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/effects"
)

func TestHTTPTransportAdaptersShareResponseAndSecurity(t *testing.T) {
	type observation struct {
		query         url.Values
		authorization string
		forwarded     string
	}
	observations := make(chan observation, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observations <- observation{
			query:         r.URL.Query(),
			authorization: r.Header.Get("Authorization"),
			forwarded:     r.Header.Get("X-Transport-Test"),
		}
		w.Header().Set("X-Transport-Result", "shared")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":3,"ok":true}`))
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	egress := effects.NewEgressPolicy([]string{serverURL.Hostname()}, true).InsecureAllowPrivateForTest()
	request := map[string]any{
		"url":     server.URL + "?from=base",
		"method":  "GET",
		"query":   map[string]any{"q": "blue host"},
		"headers": map[string]any{"Authorization": "must-not-forward", "X-Transport-Test": "kept"},
	}

	direct, err := doHTTPRequest(context.Background(), egress, request)
	if err != nil {
		t.Fatalf("direct EffectHandlers transport: %v", err)
	}
	genericResult := runHTTPEffect(context.Background(), egress, map[string]any{"request": request})
	if genericResult.Err != "" {
		t.Fatalf("generic invocation transport: %s", genericResult.Err)
	}
	genericValue, err := decodeCanonicalJSON(genericResult.Value)
	if err != nil {
		t.Fatalf("decode generic response: %v", err)
	}
	generic, ok := genericValue.(map[string]any)
	if !ok {
		t.Fatalf("generic response shape: %#v", genericValue)
	}

	for _, key := range []string{"status", "body", "headers"} {
		if !reflect.DeepEqual(direct[key], generic[key]) {
			t.Fatalf("shared transport response diverged for %q: direct=%#v generic=%#v", key, direct[key], generic[key])
		}
	}
	if ok, _ := direct["ok"].(bool); !ok {
		t.Fatalf("direct response did not retain its EffectHandlers ok output: %#v", direct["ok"])
	}

	for i := 0; i < 2; i++ {
		observation := <-observations
		if got := observation.query.Get("from"); got != "base" {
			t.Fatalf("request %d lost the original query parameter: %q", i, got)
		}
		if got := observation.query.Get("q"); got != "blue host" {
			t.Fatalf("request %d lost the authored query parameter: %q", i, got)
		}
		if observation.authorization != "" {
			t.Fatalf("request %d forwarded Authorization through the shared transport", i)
		}
		if observation.forwarded != "kept" {
			t.Fatalf("request %d dropped a permitted authored header", i)
		}
	}
}

func TestHTTPTransportAdaptersShareHeaderCap(t *testing.T) {
	egress := effects.NewEgressPolicy([]string{"allowed.example.test"}, false)
	request := map[string]any{
		"url":     "https://allowed.example.test/effect",
		"headers": map[string]any{"X-Oversized": strings.Repeat("x", maxHTTPTransportHeaders)},
	}

	if _, err := doHTTPRequest(context.Background(), egress, request); err == nil || !strings.Contains(err.Error(), "HTTP_REQUEST_HEADERS_TOO_LARGE") {
		t.Fatalf("direct adapter did not enforce the shared header cap: %v", err)
	}
	generic := runHTTPEffect(context.Background(), egress, map[string]any{"request": request})
	if !strings.Contains(generic.Err, "HTTP_REQUEST_HEADERS_TOO_LARGE") {
		t.Fatalf("generic adapter did not enforce the shared header cap: %s", generic.Err)
	}
}
