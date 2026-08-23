package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testSecret = "s3cret-handshake-from-prism"

func newLocal(t *testing.T) AuthSource {
	t.Helper()
	src, err := NewLocalOperatorAuth(testSecret, "alice")
	if err != nil {
		t.Fatalf("NewLocalOperatorAuth: %v", err)
	}
	return src
}

// TestLocalOperatorAuth_GrantsOperatorOnValidHandshake proves the
// embedded-local source grants the operator Identity (RC-4 happy path) when
// the request carries the matching handshake secret. requireOperator then
// admits it exactly as it would a ZabGate-injected operator header.
func TestLocalOperatorAuth_GrantsOperatorOnValidHandshake(t *testing.T) {
	src := newLocal(t)
	h := http.Header{HandshakeHeader: {testSecret}}

	id := src.FromHeaders(h)
	if id.Role != RoleOperator {
		t.Fatalf("role = %q, want operator", id.Role)
	}
	if !id.IsAuthenticated() {
		t.Fatal("identity not authenticated")
	}
	if id.UserID != "alice" {
		t.Fatalf("user = %q, want alice", id.UserID)
	}
}

// TestLocalOperatorAuth_GrantsScopedServiceForQuasarWriter proves the local
// Quasar bridge can remain connected without requiring an active scene while
// still being restricted to the declared platform-event input namespace.
func TestLocalOperatorAuth_GrantsScopedServiceForQuasarWriter(t *testing.T) {
	src := newLocal(t)
	h := http.Header{
		HandshakeHeader: {testSecret},
		LocalRoleHeader: {localServiceRole},
	}

	id := src.FromHeaders(h)
	if id.Role != RoleService {
		t.Fatalf("role = %q, want service", id.Role)
	}
	if !id.CanWritePath("__inputs.platform.twitch.channel.last_chat") {
		t.Fatal("service cannot write a platform event input")
	}
	if id.CanWritePath("__scene.internal") {
		t.Fatal("service can write outside the platform event namespace")
	}
}

// TestLocalOperatorAuth_RefusesWrongOrAbsentHandshake proves the source is
// fail-closed (RC-4 refusal): a missing, empty, or mismatched secret yields
// the anonymous Identity, which requireOperator rejects. This is the guard
// against another local process impersonating Prism.
func TestLocalOperatorAuth_RefusesWrongOrAbsentHandshake(t *testing.T) {
	src := newLocal(t)

	cases := map[string]http.Header{
		"absent":         {},
		"empty":          {HandshakeHeader: {""}},
		"wrong":          {HandshakeHeader: {"not-the-secret"}},
		"prefix-of-real": {HandshakeHeader: {testSecret[:5]}},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			id := src.FromHeaders(h)
			if id.IsAuthenticated() || id.Role == RoleOperator {
				t.Fatalf("granted on %s handshake: %+v", name, id)
			}
		})
	}
}

// TestLocalOperatorAuth_DoesNotTrustZabGateHeaders proves the local source
// ignores X-Authenticated-* entirely: a forged operator header without the
// handshake secret gets nothing. In embedded-local the trust comes ONLY
// from the handshake, never from spoofable role headers.
func TestLocalOperatorAuth_DoesNotTrustZabGateHeaders(t *testing.T) {
	src := newLocal(t)
	h := http.Header{
		"X-Authenticated-User": {"attacker"},
		"X-Authenticated-Role": {"operator"},
	}
	if id := src.FromHeaders(h); id.IsAuthenticated() {
		t.Fatalf("trusted a spoofed ZabGate header: %+v", id)
	}
}

// TestNewLocalOperatorAuth_RefusesEmptySecret proves boot refuses an
// embedded-local AuthSource without a handshake secret (ADR 016 §5 R2):
// otherwise it would grant operator to any loopback caller.
func TestNewLocalOperatorAuth_RefusesEmptySecret(t *testing.T) {
	if _, err := NewLocalOperatorAuth("", "alice"); err != ErrNoHandshakeSecret {
		t.Fatalf("err = %v, want ErrNoHandshakeSecret", err)
	}
}

// TestLoopbackOnly enforces the off-host refusal (ADR 016 D4): loopback
// remote addrs pass, routable ones get 403 before the handler runs.
func TestLoopbackOnly(t *testing.T) {
	var reached bool
	h := LoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name       string
		remoteAddr string
		wantCode   int
		wantReach  bool
	}{
		{"ipv4-loopback", "127.0.0.1:51234", http.StatusOK, true},
		{"ipv4-loopback-range", "127.5.6.7:80", http.StatusOK, true},
		{"ipv6-loopback", "[::1]:51234", http.StatusOK, true},
		{"routable-v4", "203.0.113.10:443", http.StatusForbidden, false},
		{"lan-v4", "192.168.1.20:443", http.StatusForbidden, false},
		{"unparseable", "garbage", http.StatusForbidden, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodGet, "/api/v1/show", nil)
			req.RemoteAddr = c.remoteAddr
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantCode {
				t.Fatalf("code = %d, want %d", rec.Code, c.wantCode)
			}
			if reached != c.wantReach {
				t.Fatalf("handler reached = %v, want %v", reached, c.wantReach)
			}
		})
	}
}
