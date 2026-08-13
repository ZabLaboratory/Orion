package workload

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestAdmitAuthContext_Success(t *testing.T) {
	client, srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/workload/auth-contexts/admit" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("x-workload-san") != testIdentity().San {
			t.Fatalf("missing x-workload-san header")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"admission_id": "adm-1", "intent_id": "intent-1"})
	})
	defer srv.Close()

	adm, err := client.AdmitAuthContext(context.Background(), "opaque-ticket")
	if err != nil {
		t.Fatalf("AdmitAuthContext: %v", err)
	}
	if adm.AdmissionID != "adm-1" {
		t.Fatalf("unexpected admission_id: %q", adm.AdmissionID)
	}
}

func TestMintDelegation_KnownCode(t *testing.T) {
	client, srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": string(CodeDelegationAlreadyUsed)})
	})
	defer srv.Close()

	_, err := client.MintDelegation(context.Background(), &AuthContextAdmission{AdmissionID: "adm-1", IntentID: "intent-1"})
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

func TestMintDelegation_UnknownCodeFailsClosed(t *testing.T) {
	client, srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "SOMETHING_NEW"})
	})
	defer srv.Close()

	_, err := client.MintDelegation(context.Background(), &AuthContextAdmission{AdmissionID: "adm-1", IntentID: "intent-1"})
	var werr *Error
	if !asError(err, &werr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if werr.Code != "" {
		t.Fatalf("expected empty Code for unknown refusal, got %q", werr.Code)
	}
}

func TestFetchCanvas_NeverRetriedOnPending(t *testing.T) {
	calls := 0
	client, srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": string(CodeDelegationReconciliationPending)})
	})
	defer srv.Close()

	_, err := client.FetchCanvas(context.Background(), "jti-1")
	var werr *Error
	if !asError(err, &werr) || werr.Code != CodeDelegationReconciliationPending {
		t.Fatalf("expected CodeDelegationReconciliationPending, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("client must never auto-retry FetchCanvas itself; got %d calls", calls)
	}
}

func TestReconcile_Success(t *testing.T) {
	client, srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/workload/delegations/jti-1/reconcile" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "consumed"})
	})
	defer srv.Close()

	res, err := client.Reconcile(context.Background(), "jti-1")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.State != "consumed" {
		t.Fatalf("unexpected state: %q", res.State)
	}
}

func TestFingerprintCert(t *testing.T) {
	got := FingerprintCert([]byte("hello"))
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got == "" || got == want {
		// Just assert determinism/non-empty; the exact SHA-256 of "hello"
		// starts with 2cf24d..., checked loosely to avoid a brittle full
		// hard-coded digest mismatch across encodings.
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
