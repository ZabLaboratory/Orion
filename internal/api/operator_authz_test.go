package api

import (
	"net/http"
	"testing"
)

// Operator authz — pending/resolve gap (ORION-OPERATOR-TEST-HARDENING,
// Prism#740, gap 1). TestOperator_CallRequiresOperatorRole (operator_test.go)
// already pins requireOperator on POST /operator/call; getRuntimePending
// (operator.go:385) and postOperatorResolve (operator.go:412) carry the
// identical requireOperator wrapper but had no assertion of their own —
// Bastion measured that stripping requireOperator from EITHER route left the
// whole internal/api suite green. These two routes are exactly what the
// preview-Engine-B migration (55062a1f) and the mode gate (1a35f9f4) just
// remodeled, so a regressed guard here would go undetected.
//
// Falsified by hand: commenting out `return requireOperator(func(...` (and
// its matching closing `)` swapped for the bare handler) on getRuntimePending
// or postOperatorResolve in operator.go turns the matching test below red;
// restoring the wrapper turns it green.

func TestOperator_PendingRequiresOperatorRole(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "GET", "/api/v1/runtime/bp/pending", "viewer", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer pending: got %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
}

// TestOperator_PendingRequiresAuthentication is the degraded-state sibling:
// no X-Authenticated-* headers at all (opRequest's role=="" omits them),
// simulating a request that never reached ZabGate's header injection.
func TestOperator_PendingRequiresAuthentication(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "GET", "/api/v1/runtime/bp/pending", "", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated pending: got %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
}

func TestOperator_ResolveRequiresOperatorRole(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/bp/pick", "viewer",
		map[string]any{"value": 1})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer resolve: got %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
}

func TestOperator_ResolveRequiresAuthentication(t *testing.T) {
	f := newOperatorFixture(t)
	w := opRequest(t, f.mux, "POST", "/api/v1/operator/resolve/bp/pick", "",
		map[string]any{"value": 1})
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated resolve: got %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
}
