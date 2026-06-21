package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/auth"
)

// withAuthSource swaps the package-level authSource for the duration of a
// test and restores it (the default HeaderAuthSource) afterwards, so the
// embedded-local seam can be exercised without leaking into other tests.
func withAuthSource(t *testing.T, src auth.AuthSource) {
	t.Helper()
	prev := authSource
	authSource = src
	t.Cleanup(func() { authSource = prev })
}

func okHandler(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

// TestRequireOperator_AntenneParity proves the default (HeaderAuthSource)
// gate is byte-for-byte the prior behaviour: ZabGate operator/admin headers
// pass, everything else 403. This is the RC-1 / invariant guard — the seam
// must not change antenne behaviour.
func TestRequireOperator_AntenneParity(t *testing.T) {
	// default authSource is HeaderAuthSource{} (no swap).
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"operator", map[string]string{"X-Authenticated-User": "u", "X-Authenticated-Role": "operator"}, http.StatusOK},
		{"admin", map[string]string{"X-Authenticated-User": "u", "X-Authenticated-Role": "admin"}, http.StatusOK},
		{"viewer", map[string]string{"X-Authenticated-User": "u", "X-Authenticated-Role": "viewer"}, http.StatusForbidden},
		{"anonymous", map[string]string{}, http.StatusForbidden},
		{"no-handshake-header-ignored", map[string]string{auth.HandshakeHeader: "anything", "X-Authenticated-User": "u", "X-Authenticated-Role": "operator"}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/show/active-scene", nil)
			for k, v := range c.hdr {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			requireOperator(okHandler)(rec, r)
			if rec.Code != c.want {
				t.Fatalf("code = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

// TestRequireOperator_EmbeddedLocalSeam proves the SAME gate, fed by
// localOperatorAuth, grants on a valid handshake and refuses without it —
// while ignoring spoofed ZabGate role headers. The gate code is identical;
// only the identity source changed (ADR 016 §3.2-2, RC-4).
func TestRequireOperator_EmbeddedLocalSeam(t *testing.T) {
	src, err := auth.NewLocalOperatorAuth("handshake-xyz", "alice")
	if err != nil {
		t.Fatalf("NewLocalOperatorAuth: %v", err)
	}
	withAuthSource(t, src)

	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"valid-handshake", map[string]string{auth.HandshakeHeader: "handshake-xyz"}, http.StatusOK},
		{"no-handshake", map[string]string{}, http.StatusForbidden},
		{"wrong-handshake", map[string]string{auth.HandshakeHeader: "nope"}, http.StatusForbidden},
		{"spoofed-role-no-handshake", map[string]string{"X-Authenticated-Role": "operator"}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/show/active-scene", nil)
			for k, v := range c.hdr {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			requireOperator(okHandler)(rec, r)
			if rec.Code != c.want {
				t.Fatalf("code = %d, want %d", rec.Code, c.want)
			}
		})
	}
}
