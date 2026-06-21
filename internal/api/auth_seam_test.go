package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/auth"
)

func okHandler(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

// run drives operatorGate (the role check requireOperator delegates to)
// with an explicit source — no mutation of the package-level authSource,
// so this never races the parallel handler tests.
func run(t *testing.T, src auth.AuthSource, hdr map[string]string) int {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/show/active-scene", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	operatorGate(src, okHandler)(rec, r)
	return rec.Code
}

// TestOperatorGate_AntenneParity proves the default (HeaderAuthSource) gate
// is byte-for-byte the prior behaviour: ZabGate operator/admin headers
// pass, everything else 403. RC-1 / invariant guard — the seam must not
// change antenne behaviour. (The handshake header is meaningless here.)
func TestOperatorGate_AntenneParity(t *testing.T) {
	src := auth.HeaderAuthSource{}
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"operator", map[string]string{"X-Authenticated-User": "u", "X-Authenticated-Role": "operator"}, http.StatusOK},
		{"admin", map[string]string{"X-Authenticated-User": "u", "X-Authenticated-Role": "admin"}, http.StatusOK},
		{"viewer", map[string]string{"X-Authenticated-User": "u", "X-Authenticated-Role": "viewer"}, http.StatusForbidden},
		{"anonymous", map[string]string{}, http.StatusForbidden},
		{"handshake-header-ignored-on-antenne", map[string]string{auth.HandshakeHeader: "x", "X-Authenticated-User": "u", "X-Authenticated-Role": "operator"}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code := run(t, src, c.hdr); code != c.want {
				t.Fatalf("code = %d, want %d", code, c.want)
			}
		})
	}
}

// TestOperatorGate_EmbeddedLocalSeam proves the SAME gate, fed by
// localOperatorAuth, grants on a valid handshake and refuses without it,
// while ignoring spoofed ZabGate role headers. Identical gate code; only
// the identity source changed (ADR 016 §3.2-2, RC-4).
func TestOperatorGate_EmbeddedLocalSeam(t *testing.T) {
	src, err := auth.NewLocalOperatorAuth("handshake-xyz", "alice")
	if err != nil {
		t.Fatalf("NewLocalOperatorAuth: %v", err)
	}
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
			if code := run(t, src, c.hdr); code != c.want {
				t.Fatalf("code = %d, want %d", code, c.want)
			}
		})
	}
}
