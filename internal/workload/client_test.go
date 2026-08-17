package workload

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func testIdentity() Identity {
	return Identity{San: "spiffe://zab/workload/orion/dev/instance-1", CertSHA256: "deadbeef"}
}

func TestNewClient_RequiresClientCert(t *testing.T) {
	plain := &http.Client{}
	if _, err := NewClient("https://gate.internal", testIdentity(), plain); err == nil {
		t.Fatal("expected error for httpClient without a client certificate")
	}
}

func TestNewClient_RequiresIdentity(t *testing.T) {
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{{0x30}}}},
	}}}
	if _, err := NewClient("https://gate.internal", Identity{}, c); err == nil {
		t.Fatal("expected error for empty identity")
	}
}

func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{{0x30}}}},
	}}}
	client, err := NewClient(srv.URL, testIdentity(), c)
	if err != nil {
		t.Fatal(err)
	}
	return client, srv
}

// decodeNumberPreserving parses raw JSON with json.Number so a float64
// round-trip cannot silently mask a large-integer mutation (deadline is
// bounded by 2^53-1 on the Gate schema).
func decodeNumberPreserving(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestMintDelegation_PostsTicketAndVerbatimIntent(t *testing.T) {
	// The intent carries a max-safe-integer deadline plus fields the Go
	// side has no model for — value-identical arrival at the Gate proves
	// the relay embeds the caller's bytes instead of reconstructing them.
	intent := json.RawMessage(`{"schema_version":"orion.scene-intent.v1","intent_id":"intent-1","idempotency_key":"idem-1","sequence":7,"stream_id":"stream-1","target":"preview","action":"prepare-preview","resolved_scene_ref":"opaque-jws","issued_at":1800000000,"deadline":9007199254740991,"correlation_id":"corr-1"}`)

	var got struct {
		Ticket string          `json:"ticket"`
		Intent json.RawMessage `json:"intent"`
	}
	client, srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/workload/delegations/mint" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("x-workload-san") != testIdentity().San {
			t.Fatalf("missing x-workload-san header")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode mint request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":       "opaque-delegation",
			"token_type":         "bearer",
			"jti":                "jti-1",
			"status":             "issued",
			"expires_at":         "2026-08-17T21:00:00Z",
			"canvas_request_key": strings.Repeat("c", 64),
			"retry_sequence":     0,
			"gate_request_id":    "3f2c8f0e-2f65-4e11-8a63-0123456789ab." + strings.Repeat("d", 64),
		})
	})
	defer srv.Close()

	out, err := client.MintDelegation(context.Background(), "opaque-ticket", intent)
	if err != nil {
		t.Fatalf("MintDelegation: %v", err)
	}
	if got.Ticket != "opaque-ticket" {
		t.Fatalf("mint must post the ticket field, got %q", got.Ticket)
	}
	// Value-exact intent, big integers included — the ticket binds these
	// values (`_assert_ticket_matches`), a mutation here is a live
	// AUTH_CONTEXT_MISMATCH.
	if !reflect.DeepEqual(
		decodeNumberPreserving(t, got.Intent),
		decodeNumberPreserving(t, intent),
	) {
		t.Fatalf("intent mutated in transit:\nsent     %s\nreceived %s", intent, got.Intent)
	}
	if out.JTI != "jti-1" || out.AccessToken != "opaque-delegation" || out.RetrySequence != 0 {
		t.Fatalf("unexpected delegation decode: %+v", out)
	}
}

func TestMintDelegation_KnownCode_GateDetailEnvelope(t *testing.T) {
	// ZabGate renders refusals as {"detail": {"code", "message"}}
	// (routes.py::workload_error_handler) — the ten §4.7 codes must be
	// recognized in THAT envelope, not only the bare {"error"} spelling.
	client, srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"detail": map[string]string{
				"code":    string(CodeDelegationAlreadyUsed),
				"message": "delegation already used",
			},
		})
	})
	defer srv.Close()

	_, err := client.MintDelegation(context.Background(), "opaque-ticket", json.RawMessage(`{}`))
	var werr *Error
	if err == nil {
		t.Fatal("expected error")
	}
	if !asError(err, &werr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if werr.Code != CodeDelegationAlreadyUsed {
		t.Fatalf("unexpected code: %q", werr.Code)
	}
}

func TestMintDelegation_KnownCode_LegacyErrorEnvelope(t *testing.T) {
	client, srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": string(CodeDelegationAlreadyUsed)})
	})
	defer srv.Close()

	_, err := client.MintDelegation(context.Background(), "opaque-ticket", json.RawMessage(`{}`))
	var werr *Error
	if !asError(err, &werr) || werr.Code != CodeDelegationAlreadyUsed {
		t.Fatalf("expected CodeDelegationAlreadyUsed from the legacy envelope, got %v", err)
	}
}

func TestMintDelegation_UnknownCodeFailsClosed(t *testing.T) {
	client, srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"detail": map[string]string{"code": "SOMETHING_NEW", "message": "?"},
		})
	})
	defer srv.Close()

	_, err := client.MintDelegation(context.Background(), "opaque-ticket", json.RawMessage(`{}`))
	var werr *Error
	if !asError(err, &werr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if werr.Code != "" {
		t.Fatalf("expected empty Code for unknown refusal, got %q", werr.Code)
	}
}

func testDelegation() *Delegation {
	return &Delegation{
		JTI:              "jti-1",
		AccessToken:      "opaque-delegation",
		TokenType:        "bearer",
		Status:           "issued",
		CanvasRequestKey: strings.Repeat("c", 64),
		GateRequestID:    "3f2c8f0e-2f65-4e11-8a63-0123456789ab." + strings.Repeat("d", 64),
	}
}

// verbatimIntent carries a max-safe-integer deadline so a float64
// round-trip anywhere in the relay would be caught by the json.Number
// comparison.
var verbatimIntent = json.RawMessage(`{"schema_version":"orion.scene-intent.v1","intent_id":"intent-1","sequence":7,"deadline":9007199254740991,"correlation_id":"corr-1"}`)

func consumedProxyResponse(result map[string]any) map[string]any {
	return map[string]any{
		"jti":                "jti-1",
		"status":             "consumed",
		"canvas_request_key": strings.Repeat("c", 64),
		"result":             result,
	}
}

func TestFetchCanvas_PostsDelegationProxyRequestVerbatim(t *testing.T) {
	var got struct {
		AccessToken   string          `json:"access_token"`
		Ticket        string          `json:"ticket"`
		Intent        json.RawMessage `json:"intent"`
		GateRequestID string          `json:"gate_request_id"`
	}
	client, srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/workload/delegations/jti-1/canvas" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode proxy request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(consumedProxyResponse(map[string]any{
			"status_code":  200,
			"content_type": "application/json",
			"body":         map[string]any{"blue_program": "AAAA"},
		}))
	})
	defer srv.Close()

	artifact, err := client.FetchCanvas(context.Background(), testDelegation(), "opaque-ticket", verbatimIntent)
	if err != nil {
		t.Fatalf("FetchCanvas: %v", err)
	}
	// Field provenance: access_token/gate_request_id from the mint's
	// delegation, ticket/intent from the Prism relay — none invented.
	if got.AccessToken != "opaque-delegation" || got.Ticket != "opaque-ticket" || got.GateRequestID != testDelegation().GateRequestID {
		t.Fatalf("proxy request fields wrong: %+v", got)
	}
	if !reflect.DeepEqual(
		decodeNumberPreserving(t, got.Intent),
		decodeNumberPreserving(t, verbatimIntent),
	) {
		t.Fatalf("intent mutated in transit:\nsent     %s\nreceived %s", verbatimIntent, got.Intent)
	}
	// The artifact is the CANVAS outcome (result.status_code/result.body),
	// not the Gate transport status.
	if artifact.Status != 200 || string(artifact.Body) != `{"blue_program":"AAAA"}` {
		t.Fatalf("unexpected artifact: status=%d body=%s", artifact.Status, artifact.Body)
	}
}

func TestFetchCanvas_NeverRetriedOnPending(t *testing.T) {
	calls := 0
	client, srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"detail": map[string]string{"code": string(CodeDelegationReconciliationPending), "message": "pending"},
		})
	})
	defer srv.Close()

	_, err := client.FetchCanvas(context.Background(), testDelegation(), "opaque-ticket", verbatimIntent)
	var werr *Error
	if !asError(err, &werr) || werr.Code != CodeDelegationReconciliationPending {
		t.Fatalf("expected CodeDelegationReconciliationPending, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("client must never auto-retry FetchCanvas itself; got %d calls", calls)
	}
}

func TestFetchCanvas_UnexpectedSuccessStatusFailsClosed(t *testing.T) {
	client, srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jti": "jti-1", "status": "reserved",
			"canvas_request_key": strings.Repeat("c", 64),
			"result":             map[string]any{},
		})
	})
	defer srv.Close()

	_, err := client.FetchCanvas(context.Background(), testDelegation(), "opaque-ticket", verbatimIntent)
	var werr *Error
	if !asError(err, &werr) || werr.Code != "" {
		t.Fatalf("expected a fail-closed generic *Error for a non-consumed 200, got %v", err)
	}
}

func TestFetchCanvas_RequiresMintedDelegationFields(t *testing.T) {
	calls := 0
	client, srv := newTestServer(t, func(http.ResponseWriter, *http.Request) { calls++ })
	defer srv.Close()

	for _, d := range []*Delegation{
		nil,
		{JTI: "jti-1"}, // no access_token / gate_request_id
		{JTI: "jti-1", AccessToken: "opaque-delegation"}, // no gate_request_id
	} {
		if _, err := client.FetchCanvas(context.Background(), d, "opaque-ticket", verbatimIntent); err == nil {
			t.Fatalf("expected an error for incomplete delegation %+v", d)
		}
	}
	if calls != 0 {
		t.Fatalf("an incomplete delegation must never reach the wire, got %d calls", calls)
	}
}

func TestReconcile_Success(t *testing.T) {
	var got struct {
		AccessToken   string          `json:"access_token"`
		Ticket        string          `json:"ticket"`
		Intent        json.RawMessage `json:"intent"`
		GateRequestID string          `json:"gate_request_id"`
	}
	client, srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/workload/delegations/jti-1/reconcile" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode reconcile request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(consumedProxyResponse(map[string]any{
			"status_code":  200,
			"content_type": "application/json",
			"body":         map[string]any{"observed": true},
		}))
	})
	defer srv.Close()

	res, err := client.Reconcile(context.Background(), testDelegation(), "opaque-ticket", verbatimIntent)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.AccessToken != "opaque-delegation" || got.Ticket != "opaque-ticket" || got.GateRequestID != testDelegation().GateRequestID {
		t.Fatalf("reconcile request fields wrong: %+v", got)
	}
	if res.JTI != "jti-1" || res.CanvasRequestKey != strings.Repeat("c", 64) || string(res.Result) != `{"observed":true}` {
		t.Fatalf("unexpected reconcile result: %+v", res)
	}
}

func TestFingerprintCert(t *testing.T) {
	got := FingerprintCert([]byte("hello"))
	if got != FingerprintCert([]byte("hello")) {
		t.Fatal("expected FingerprintCert to be deterministic")
	}
	if len(got) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(got))
	}
}

func asError(err error, target **Error) bool {
	if e, ok := err.(*Error); ok {
		*target = e
		return true
	}
	return false
}
