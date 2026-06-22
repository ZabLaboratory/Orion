package lsdp

import (
	"net/http"
	"testing"

	lproto "github.com/Lumencast/lumencast-go/protocol"
	lserver "github.com/Lumencast/lumencast-go/server"

	"github.com/ZabLaboratory/Orion/internal/auth"
)

// TestIdentityFromRequest_EmbeddedLocalHandshake proves the LSDP identity
// seam honours the embedded-local AuthSource (localOperatorAuth): a request
// carrying the loopback handshake header X-Orion-Local-Auth is mapped to a
// kit operator identity, so the .lsdp route accepts the Prism cockpit's
// real Solar runtime in the embedded-local profile. Before the fix the
// seam read the static auth.FromHeaders, which sees no X-Authenticated-*
// header on the local path → Anonymous → kit closes AUTH_DENIED.
func TestIdentityFromRequest_EmbeddedLocalHandshake(t *testing.T) {
	const secret = "prism-lsdp-handshake-secret"
	src, err := auth.NewLocalOperatorAuth(secret, "local-operator")
	if err != nil {
		t.Fatal(err)
	}
	derive := identityFromRequest(src)

	// Correct handshake → operator.
	r := &http.Request{Header: http.Header{auth.HandshakeHeader: []string{secret}}}
	id, err := derive(r)
	if err != nil {
		t.Fatal(err)
	}
	if !id.IsAuthenticated() {
		t.Fatal("expected authenticated identity for valid handshake")
	}
	if id.Role != lproto.RoleOperator {
		t.Fatalf("role = %q, want operator", id.Role)
	}
	if id.Subject != "local-operator" {
		t.Fatalf("subject = %q, want local-operator", id.Subject)
	}

	// Wrong secret → Anonymous (kit closes AUTH_DENIED). Fail-closed.
	rBad := &http.Request{Header: http.Header{auth.HandshakeHeader: []string{"wrong"}}}
	idBad, err := derive(rBad)
	if err != nil {
		t.Fatal(err)
	}
	if idBad.IsAuthenticated() {
		t.Fatal("wrong handshake must yield anonymous identity")
	}

	// No header at all → Anonymous.
	idNone, err := derive(&http.Request{Header: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	if idNone.IsAuthenticated() {
		t.Fatal("absent handshake must yield anonymous identity")
	}
}

// TestIdentityFromRequest_AntenneHeaderTrust proves prod parity: with the
// default HeaderAuthSource (the antenne profile, and the nil-default), the
// ZabGate-injected X-Authenticated-* headers still map to the same kit
// identity as before — operator, viewer, service with paths — and an
// unauthenticated request still yields Anonymous.
func TestIdentityFromRequest_AntenneHeaderTrust(t *testing.T) {
	// nil source must default to HeaderAuthSource (byte-for-byte parity).
	for name, derive := range map[string]func(*http.Request) (lserver.Identity, error){
		"explicit-header-source": identityFromRequest(auth.HeaderAuthSource{}),
		"nil-defaults-to-header": identityFromRequest(nil),
	} {
		t.Run(name, func(t *testing.T) {
			// Operator via trust headers.
			op := &http.Request{Header: http.Header{
				"X-Authenticated-User": []string{"user-1"},
				"X-Authenticated-Role": []string{"operator"},
			}}
			id, err := derive(op)
			if err != nil {
				t.Fatal(err)
			}
			if id.Role != lproto.RoleOperator || id.Subject != "user-1" {
				t.Fatalf("operator mapping wrong: %+v", id)
			}

			// Viewer (Pulsar CEF show-token → ZabGate viewer header).
			vw := &http.Request{Header: http.Header{
				"X-Authenticated-User": []string{"pulsar-cef"},
				"X-Authenticated-Role": []string{"viewer"},
			}}
			idv, err := derive(vw)
			if err != nil {
				t.Fatal(err)
			}
			if idv.Role != lproto.RoleViewer {
				t.Fatalf("viewer mapping wrong: %+v", idv)
			}

			// Service with allow-list paths preserved.
			svc := &http.Request{Header: http.Header{
				"X-Authenticated-User":  []string{"quasar"},
				"X-Authenticated-Role":  []string{"service"},
				"X-Authenticated-Paths": []string{"score.team_a,score.team_b"},
			}}
			ids, err := derive(svc)
			if err != nil {
				t.Fatal(err)
			}
			if ids.Role != lproto.RoleService || len(ids.Paths) != 2 {
				t.Fatalf("service mapping wrong: %+v", ids)
			}

			// No trust header → Anonymous (kit AUTH_DENIED).
			anon, err := derive(&http.Request{Header: http.Header{}})
			if err != nil {
				t.Fatal(err)
			}
			if anon.IsAuthenticated() {
				t.Fatalf("anonymous expected, got %+v", anon)
			}

			// A loopback handshake header must NOT grant operator on the
			// antenne path — HeaderAuthSource ignores it entirely.
			spoof := &http.Request{Header: http.Header{auth.HandshakeHeader: []string{"anything"}}}
			sp, err := derive(spoof)
			if err != nil {
				t.Fatal(err)
			}
			if sp.IsAuthenticated() {
				t.Fatal("handshake header must be inert under HeaderAuthSource (antenne)")
			}
		})
	}
}
