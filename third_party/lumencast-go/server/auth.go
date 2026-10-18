// Package server is the Lumencast server kit. It provides a
// WebSocket subscription handler, a leaf-grain scene store, role
// enforcement, and adapter helpers — wrapping the LSDP/1 codec from
// the protocol package.
//
// The kit is opinionated about the wire format (it implements LSDP/1
// verbatim) and unopinionated about everything else : credentials,
// persistence, business logic. The user provides an Authenticator
// implementation ; the kit calls it on every WebSocket upgrade.
package server

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/Lumencast/lumencast-go/protocol"
)

// Identity is the principal returned by an Authenticator after a
// successful token validation, or by a RequestIdentityFunc derived
// from trusted request headers (ADR 007 §C.3a, no token involved).
// The server uses it to enforce role and path scoping on incoming
// Input frames.
type Identity struct {
	// Subject is the human-readable principal identifier — a user
	// name, a service name. Used in audit logs ; not load-bearing
	// for authorisation decisions.
	Subject string

	// Role drives the authorisation matrix (see protocol.Role).
	Role protocol.Role

	// Paths optionally restricts a service token to a subset of the
	// __inputs.* tree. Empty Paths on a service token means no
	// restriction beyond the role default. Patterns may end with
	// `*` to match any descendant.
	Paths []string
}

// Anonymous returns an unauthenticated Identity. Authenticators that
// reject a token MUST return (Anonymous, an error).
func Anonymous() Identity { return Identity{} }

// IsAuthenticated reports whether the identity carries a non-empty
// role. The empty Identity is considered anonymous.
func (i Identity) IsAuthenticated() bool {
	return i.Role.IsValid()
}

// CanWrite reports whether the identity is allowed to mutate the given
// leaf path. Operators write everywhere under __inputs.* ; service
// tokens are scoped to their Paths claim ; test-mode connections
// write __test.*.
//
// This is the role/scope gate. The server applies an *additional*
// check that the path is declared in the active scene's
// operator_inputs ; that one lives in Scene.applyInput.
func (i Identity) CanWrite(path string) bool {
	switch i.Role {
	case protocol.RoleOperator:
		return protocol.LeafPath(path).HasPrefix("__inputs")
	case protocol.RoleService:
		if !protocol.LeafPath(path).HasPrefix("__inputs") {
			return false
		}
		if len(i.Paths) == 0 {
			return true
		}
		for _, prefix := range i.Paths {
			if matchPathPattern(prefix, path) {
				return true
			}
		}
		return false
	case protocol.RoleTest:
		return protocol.LeafPath(path).HasPrefix("__test")
	default:
		return false
	}
}

// matchPathPattern compares a service-token path pattern against a
// concrete path. `*` and `.*` suffixes match anything below ; bare
// patterns match exact + segment-aware prefix.
func matchPathPattern(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	if pattern == path {
		return true
	}
	if len(pattern) >= 2 && pattern[len(pattern)-2:] == ".*" {
		return protocol.LeafPath(path).HasPrefix(pattern[:len(pattern)-2])
	}
	if pattern[len(pattern)-1] == '*' {
		// "__inputs.*" → match any path under __inputs
		root := pattern[:len(pattern)-1]
		if root != "" && root[len(root)-1] == '.' {
			return protocol.LeafPath(path).HasPrefix(root[:len(root)-1])
		}
	}
	return protocol.LeafPath(path).HasPrefix(pattern)
}

// Authenticator validates a token and yields an Identity. It is the
// single extension point for credential handling. Production
// implementations typically wrap a JWT verifier, an mTLS-derived
// principal, or a remote token-introspection endpoint.
type Authenticator interface {
	// Authenticate is called on every WebSocket subscribe frame.
	// The supplied token comes verbatim from the wire ; the
	// implementation MUST validate it. Returning a nil error AND
	// an Identity with an invalid role is treated as authentication
	// failure.
	Authenticate(ctx context.Context, token string) (Identity, error)
}

// RequestIdentityFunc derives an Identity from the HTTP request that
// carried the WebSocket upgrade, instead of from the Subscribe frame's
// token. It is the header-trust seam (ADR 007 §C.3a) : a trusted
// front-proxy or gateway authenticates the caller and injects the
// principal as request headers, and the server reads it from there.
//
// When configured on Config.IdentityFromRequest, the server calls this
// on every upgrade and IGNORES Subscribe.Token. Returning a non-nil
// error, or an Identity with an invalid role, is treated as
// authentication failure (the server replies AUTH_DENIED and closes).
//
// It is mutually additive with Auth : if both are set, the request
// function wins. The token path (Auth only) is unchanged.
type RequestIdentityFunc func(r *http.Request) (Identity, error)

// AuthenticatorFunc adapts an ordinary function to the Authenticator
// interface for one-off use. Useful in tests and the lumencast init
// template.
type AuthenticatorFunc func(ctx context.Context, token string) (Identity, error)

// Authenticate implements Authenticator.
func (f AuthenticatorFunc) Authenticate(ctx context.Context, token string) (Identity, error) {
	return f(ctx, token)
}

// ErrAuthDenied is the canonical authentication failure. Wraps it
// from your Authenticator when you want the server to reply with
// AUTH_DENIED on the wire.
var ErrAuthDenied = errors.New("auth: token invalid, expired, or revoked")

// StaticTokens is a development-only Authenticator that maps fixed
// token strings to identities. The lumencast init template flags it
// as a TODO ; do not deploy this to production.
type StaticTokens struct {
	mu     sync.RWMutex
	tokens map[string]Identity
}

// NewStaticTokens returns a StaticTokens authenticator pre-populated
// with the given map. The map is copied — later mutations to the
// caller's map have no effect.
func NewStaticTokens(tokens map[string]Identity) *StaticTokens {
	cp := make(map[string]Identity, len(tokens))
	for k, v := range tokens {
		cp[k] = v
	}
	return &StaticTokens{tokens: cp}
}

// Set adds or replaces a token mapping. Safe for concurrent use.
func (s *StaticTokens) Set(token string, id Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = id
}

// Delete removes a token mapping. Safe for concurrent use.
func (s *StaticTokens) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

// Reset removes every token mapping. Safe for concurrent use. Used by
// the interop control plane between scenarios.
func (s *StaticTokens) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]Identity{}
}

// Authenticate implements Authenticator.
func (s *StaticTokens) Authenticate(_ context.Context, token string) (Identity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.tokens[token]
	if !ok {
		return Anonymous(), ErrAuthDenied
	}
	return id, nil
}
