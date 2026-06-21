// Package auth handles request-level identity. Per ADR 002 § 10 and
// ADR 004 § 6.1, ZabGate has already validated the JWT before any
// request reaches Orion; we read the trust headers it injected.
//
// Two trust paths land here:
//
//   - HTTP and operator/service WS: ZabGate sets X-Authenticated-User
//     and X-Authenticated-Role on the upstream request.
//   - Pulsar CEF show-token (query string): ZabGate also validates
//     and injects the same headers, so Orion's WS handler reads the
//     same surface regardless of how the client authed.
//
// The Validator wired in this package is for an extra defence in
// depth: when Orion is *handed* a show-token (e.g., received via
// query string at the ZabGate boundary), it re-checks revocation
// against ZabAuth's /tokens/{jti}/validate endpoint. Cache TTL is
// short (default 60 s) so a revoke propagates quickly.
package auth

import (
	"errors"
	"net/http"
	"strings"
)

// Role mirrors the JWT `role` claim. Stable string set used in scope
// checks across WS and HTTP.
type Role string

const (
	RoleAnonymous Role = ""
	RoleViewer    Role = "viewer"
	RoleOperator  Role = "operator"
	RoleService   Role = "service"
	RoleAdmin     Role = "admin"
)

// Identity is the authenticated principal for a request, derived
// purely from ZabGate-injected headers (and, for show-tokens reached
// via WS query string, from a successful /validate call).
type Identity struct {
	UserID string   // X-Authenticated-User
	Role   Role     // X-Authenticated-Role
	Paths  []string // service-token `paths` claim, populated only when Role == RoleService
	JTI    string   // optional — present when the source was a show-token validated via /validate
}

// FromHeaders builds an Identity from a request's headers. Returns
// the anonymous identity if no trust headers are present (the caller
// decides whether to reject).
func FromHeaders(h http.Header) Identity {
	id := Identity{
		UserID: h.Get("X-Authenticated-User"),
		Role:   Role(strings.ToLower(h.Get("X-Authenticated-Role"))),
	}
	if raw := h.Get("X-Authenticated-Paths"); raw != "" {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				id.Paths = append(id.Paths, p)
			}
		}
	}
	return id
}

// AuthSource derives the request principal (ADR 016 §3.2). It is the
// single seam through which Orion turns an inbound request into an
// Identity. The antenne profile uses HeaderAuthSource (reads the
// X-Authenticated-* headers ZabGate injected); the embedded-local
// profile (issue #223) will plug a loopback handshake source behind
// the same interface. requireOperator and every other consumer keep
// reading an Identity exactly as today — only who derives it changes.
//
// The name is fixed by ADR 016 §3.2 (the "AuthSource" abstraction); the
// auth.AuthSource stutter is accepted deliberately to match the contract.
//
//nolint:revive // ADR 016 §3.2 names this abstraction AuthSource.
type AuthSource interface {
	FromHeaders(h http.Header) Identity
}

// HeaderAuthSource is the antenne-profile default: it trusts the
// X-Authenticated-* headers injected by ZabGate after JWT validation
// (architecture.md trust model). It is a thin, stateless adapter over
// the package-level FromHeaders so the existing hot-path call sites
// stay byte-for-byte identical.
type HeaderAuthSource struct{}

// compile-time assertion: HeaderAuthSource satisfies AuthSource.
var _ AuthSource = HeaderAuthSource{}

// FromHeaders delegates to the package-level FromHeaders, preserving
// the exact antenne behaviour.
func (HeaderAuthSource) FromHeaders(h http.Header) Identity { return FromHeaders(h) }

// IsAuthenticated reports whether the identity carries any role
// signal. Used by API handlers to gate routes that demand a real
// principal.
func (i Identity) IsAuthenticated() bool {
	return i.UserID != "" && i.Role != RoleAnonymous
}

// CanWritePath reports whether this identity is allowed to write to
// the given dotted state path. Operators and admins write everywhere
// (Logic-declared paths only — that gate sits in the runtime). Service
// tokens are scoped to their `paths` prefix list. Viewers never write.
//
// The runtime layer applies the *additional* check that the path is
// declared in the scene's operator_inputs / external_adapters; this
// method is only the role/scope gate.
func (i Identity) CanWritePath(path string) bool {
	switch i.Role {
	case RoleOperator, RoleAdmin:
		return true
	case RoleService:
		for _, prefix := range i.Paths {
			if matchPath(prefix, path) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// matchPath compares a prefix pattern (`__inputs.platform.*`) against
// a concrete dotted path (`__inputs.platform.twitch.zabchannel.last_chat`).
// Patterns may end with `*` to match anything below; otherwise they
// must equal the leaf or be a strict prefix terminated by `.`.
func matchPath(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	if pattern == path {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "*"))
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "*"))
	}
	return strings.HasPrefix(path, pattern+".")
}

// ErrUnauthenticated is returned by middleware when a route requires
// an authenticated identity but none was injected by ZabGate.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// ErrForbidden is returned when the identity is authenticated but
// lacks the required role.
var ErrForbidden = errors.New("auth: forbidden")
