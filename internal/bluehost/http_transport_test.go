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

// TestHTTPTransportMethodNormalization proves, by execution, the `method`
// parity table Blue copied into HttpRequestPayload (Blue PR #330, mirroring
// http_transport.go:78-81 + the token-validation follow-through at ~:101).
// Orion#385: the Blue side of this contract was proven by Python unit tests
// only — nothing on the Go side asserted `method` at all. Every assertion
// here reads the method the httptest.Server actually received on the wire
// (or the absence of any request, for the refused case), never a value the
// test recomputes itself — a self-referential oracle would prove nothing.
func TestHTTPTransportMethodNormalization(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		onWire  string // expected r.Method; ignored when refused is true
		refused bool   // true: the transport must error and the server must never see a request
	}{
		{name: "empty defaults to GET", method: "", onWire: http.MethodGet},
		{name: "lowercase get uppercases", method: "get", onWire: http.MethodGet},
		{name: "mixed case uppercases", method: "GeT", onWire: http.MethodGet},
		{name: "lowercase post uppercases", method: "post", onWire: http.MethodPost},
		{name: "non-standard verb PURGE is not allowlisted away", method: "PURGE", onWire: "PURGE"},
		{name: "non-standard verb PROPFIND is not allowlisted away", method: "PROPFIND", onWire: "PROPFIND"},
		{name: "padded method is refused, never reaches the wire", method: " get ", refused: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Method
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			serverURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			egress := effects.NewEgressPolicy([]string{serverURL.Hostname()}, true).InsecureAllowPrivateForTest()
			request := map[string]any{"url": server.URL, "method": tc.method}

			_, err = doHTTPRequest(context.Background(), egress, request)

			if tc.refused {
				if err == nil {
					t.Fatalf("method %q: expected the transport to refuse it, got no error", tc.method)
				}
				select {
				case got := <-received:
					t.Fatalf("method %q: transport returned an error (%v) but the server still received a request with method %q — it was emitted anyway", tc.method, err, got)
				default:
				}
				return
			}

			if err != nil {
				t.Fatalf("method %q: unexpected transport error: %v", tc.method, err)
			}
			select {
			case got := <-received:
				if got != tc.onWire {
					t.Fatalf("method %q: server observed method %q on the wire, want %q", tc.method, got, tc.onWire)
				}
			default:
				t.Fatalf("method %q: server never received a request", tc.method)
			}
		})
	}
}
