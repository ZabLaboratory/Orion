package auth

import (
	"crypto/subtle"
	"net/http"

	"github.com/ZabLaboratory/Orion/internal/obs"
)

// LocalViewerQuery authenticates a browser WebSocket against the same local
// operator source without exposing the operator handshake in the URL. Prism
// verifies the random viewer token before this middleware injects the
// loopback-only handshake header into a cloned request.
func LocalViewerQuery(viewerToken, operatorSecret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.URL.Query().Get("local_viewer_token")
		if viewerToken == "" || operatorSecret == "" ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(viewerToken)) != 1 {
			next.ServeHTTP(w, r)
			return
		}
		clone := r.Clone(r.Context())
		clone.Header.Set(HandshakeHeader, operatorSecret)
		next.ServeHTTP(w, clone)
	})
}

// HandshakeHeader is the request header carrying the Prism↔Orion shared
// handshake secret (ADR 016 §3.2-2, §3.4). In the embedded-local profile
// the Prism main process generates a high-entropy secret at sidecar spawn,
// passes it to the Orion binary via ORION_LOCAL_OPERATOR_SECRET, and sets it
// on every loopback request it issues. Orion grants the operator role
// ONLY when this header matches — so another local process that finds the
// loopback port cannot impersonate Prism and obtain operator (D4, R2).
const HandshakeHeader = "X-Orion-Local-Auth" //nolint:gosec // header name, not a credential.

// LocalRoleHeader lets Prism distinguish its background Quasar event writer
// from the local desktop operator. It is meaningful only after the same
// loopback handshake has succeeded; the header alone never authenticates a
// caller.
const LocalRoleHeader = "X-Orion-Local-Role"

const localServiceRole = "service"

// localOperatorAuth is the embedded-local AuthSource (ADR 016 §3.2-2). It
// is the SECOND implementation of AuthSource; it is wired at boot ONLY
// when ORION_PROFILE=embedded-local, and never reached on the antenne path
// (the profile branch in main.go selects HeaderAuthSource otherwise).
//
// Threat model it answers (ADR 016 §5 R2, §7 RC-4):
//
//   - The single-binary Prism sidecar has no ZabGate/ZabAuth in front of
//     it, so the X-Authenticated-* trust headers HeaderAuthSource relies on
//     are never injected. Without a local source, requireOperator would
//     reject every cockpit / /operator/* request — the local cockpit would
//     be dead.
//   - localOperatorAuth supplies an operator Identity for the local user,
//     but ONLY for requests that present the correct handshake secret. The
//     role semantics are preserved BIT-FOR-BIT: requireOperator runs the
//     exact same code, it just gets its Identity from here instead of from
//     ZabGate headers (D2). Only WHO derives the Identity changes.
//
// Two independent guards together make this safe (defence in depth):
//
//  1. Loopback-only listen (config.go pins 127.0.0.1, ADR 016 D4) PLUS the
//     loopbackOnly middleware (this package) reject any off-host socket —
//     the operator grant is never offered on a non-loopback connection.
//  2. The handshake secret gates the grant per-request, so a DIFFERENT
//     local process that finds the loopback port still cannot get operator
//     without the secret Prism holds.
//
// A request without (or with a wrong) handshake secret falls back to the
// anonymous Identity, which requireOperator rejects with 403 — fail-closed.
type localOperatorAuth struct {
	// secret is the Prism↔Orion handshake secret, compared in constant
	// time. Never empty in a correctly-wired embedded-local boot (the
	// constructor refuses an empty secret).
	secret []byte
	// localUserID is the X-Authenticated-User equivalent for the local
	// principal (the local desktop user). Cosmetic — role is what gates.
	localUserID string
}

// compile-time assertion: localOperatorAuth satisfies AuthSource.
var _ AuthSource = (*localOperatorAuth)(nil)

// NewLocalOperatorAuth builds the embedded-local AuthSource. It returns
// ErrNoHandshakeSecret if secret is empty: an embedded-local boot without
// a handshake secret would grant operator to any loopback caller, which is
// exactly the hole R2 forbids, so we refuse to start instead.
func NewLocalOperatorAuth(secret, localUserID string) (AuthSource, error) {
	if secret == "" {
		return nil, ErrNoHandshakeSecret
	}
	if localUserID == "" {
		localUserID = "local-operator"
	}
	return &localOperatorAuth{secret: []byte(secret), localUserID: localUserID}, nil
}

// FromHeaders grants the operator Identity when the request carries the
// matching handshake secret, else the anonymous Identity (rejected by
// requireOperator). The comparison is constant-time to avoid leaking the
// secret through timing. Note this trusts that the connection is already
// loopback-bound (guard #1); the secret is guard #2.
func (l *localOperatorAuth) FromHeaders(h http.Header) Identity {
	got := h.Get(HandshakeHeader)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), l.secret) != 1 {
		return Identity{} // anonymous — fail-closed.
	}
	if h.Get(LocalRoleHeader) == localServiceRole {
		return Identity{
			UserID: l.localUserID,
			Role:   RoleService,
			Paths:  []string{"__inputs.platform.*"},
		}
	}
	return Identity{
		UserID: l.localUserID,
		Role:   RoleOperator,
	}
}

// LoopbackOnly wraps a handler and rejects any request whose remote
// address is not loopback (127.0.0.0/8 or ::1). It is the in-process
// complement to the loopback-only listen address (config.go, ADR 016 D4):
// even if the binary were ever mis-bound to a routable interface, the
// operator grant would still never be served off-host. Wired ONLY in the
// embedded-local profile — the antenne path never sees this middleware.
func LoopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemote(r.RemoteAddr) {
			obs.WritePrismHTTPError(w, http.StatusForbidden, "PERMISSION_DENIED", "embedded-local: non-loopback request refused", "orion.local-auth")
			return
		}
		next.ServeHTTP(w, r)
	})
}
