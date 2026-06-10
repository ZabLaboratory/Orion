package effects

// Probe tests for the HTTP egress policy (ADR 003 §3.1.6, R2/B1, issue #85).
// Complement Forge's egress_test.go — never rewrite it.
//
// Axes:
//   1. IPv4-mapped IPv6 form of 169.254.169.254 is blocked at dial
//   2. Redirect to an internal host is blocked hop-by-hop
//   3. Redirect that changes scheme to ftp:// is blocked
//   4. InsecureAllowPrivateForTest is unreachable from non-test callers
//      (package guard: NewEgressPolicy leaves insecureAllowPrivate=false)
//   5. Empty host in URL is denied
//   6. All IPs blocked → ErrEgressBlocked (not nil, not panic)
//   7. Allowlist host with explicit port still passes URL check

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestEgress_IPv4MappedIPv6MetadataBlocked: the 169.254.169.254 metadata
// address in IPv4-mapped-IPv6 form (::ffff:169.254.169.254) is link-local
// and must be blocked by blockedIP — the IP vetting is address-family
// agnostic.
func TestEgress_IPv4MappedIPv6MetadataBlocked(t *testing.T) {
	cases := []string{
		"::ffff:169.254.169.254",
		"::ffff:a9fe:a9fe", // same address in compact hex
		"::ffff:10.0.0.1",  // RFC1918 via IPv4-mapped
		"::ffff:192.168.1.1",
	}
	for _, s := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("net.ParseIP(%q) returned nil — fix the test address", s)
		}
		if !blockedIP(ip) {
			t.Errorf("IPv4-mapped %s must be blocked (link-local / private)", s)
		}
	}
}

// TestEgress_RedirectToUnlistedHostBlocked: an allowlisted origin returns a
// redirect whose Location targets a hostname that is NOT in the allowlist —
// the hop check in CheckRedirect must catch it before a dial happens.
// (A separate redirect-to-internal test is in Forge's egress_test.go.)
func TestEgress_RedirectToUnlistedHostBlocked(t *testing.T) {
	// Origin server redirects to a hostname that is NOT in the allowlist.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://not-in-allowlist.example.com/secret", http.StatusFound)
	}))
	defer origin.Close()

	originHost := mustParse(t, origin.URL).Hostname()
	p := NewEgressPolicy([]string{originHost}, true).InsecureAllowPrivateForTest()
	resp, err := p.Client().Get(origin.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("redirect to unlisted host must fail")
	}
	if !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("expected ErrEgressBlocked on redirect to unlisted host, got %v", err)
	}
}

// TestEgress_RedirectSchemeDowngradeBlocked: a redirect response from an
// allowlisted https origin that sends Location with ftp:// scheme must be
// blocked by the redirect re-check (ftp is never allowed).
func TestEgress_RedirectSchemeDowngradeBlocked(t *testing.T) {
	p := NewEgressPolicy([]string{"api.example.com"}, false)
	// Simulate what CheckRedirect would receive for a ftp:// Location.
	ftpURL, _ := url.Parse("ftp://api.example.com/file")
	req := &http.Request{URL: ftpURL}
	err := p.CheckURL(req.URL)
	if !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("ftp:// redirect hop must be blocked, got %v", err)
	}
}

// TestEgress_ProductionConstructorNeverSetsInsecureFlag: NewEgressPolicy
// always produces a policy with insecureAllowPrivate=false — the only way
// to set it is InsecureAllowPrivateForTest(), which is TEST-ONLY. This test
// pins the invariant so a future refactor cannot accidentally wire it to
// configuration.
func TestEgress_ProductionConstructorNeverSetsInsecureFlag(t *testing.T) {
	p := NewEgressPolicy([]string{"api.example.com"}, false)
	if p.insecureAllowPrivate {
		t.Fatal("NewEgressPolicy must never set insecureAllowPrivate=true — " +
			"that field is test-only and must not be reachable from configuration")
	}
	// Verify the dial DOES block a loopback address on this policy.
	_, err := p.dialContext(context.Background(), "tcp", "127.0.0.1:443")
	if !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("loopback must be blocked on a production policy, got %v", err)
	}
}

// TestEgress_EmptyHostDenied: a URL with an empty host (e.g. a bare
// scheme) is rejected before DNS — no allowlist match can cover it.
func TestEgress_EmptyHostDenied(t *testing.T) {
	p := NewEgressPolicy([]string{""}, false)
	if err := p.CheckURL(mustParse(t, "https:///path")); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("URL with empty host must be blocked, got %v", err)
	}
}

// TestEgress_AllIPsPrivate_ReturnsErrEgressBlocked: when the DNS resolver
// returns only blocked addresses (all private/loopback) dialContext must
// return an ErrEgressBlocked-wrapped error, not nil, not a different error.
func TestEgress_AllIPsPrivate_ReturnsErrEgressBlocked(t *testing.T) {
	p := NewEgressPolicy([]string{"allowed.example.com"}, false)
	p.SetLookupForTest(func(_ context.Context, _ string) ([]net.IPAddr, error) {
		// All candidates are private — every one is vetoed.
		return []net.IPAddr{
			{IP: net.ParseIP("10.0.0.1")},
			{IP: net.ParseIP("192.168.0.5")},
		}, nil
	})
	_, err := p.dialContext(context.Background(), "tcp", "allowed.example.com:443")
	if !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("all-private IPs must yield ErrEgressBlocked, got %v", err)
	}
}

// TestEgress_HostWithExplicitPortPassesURLCheck: the allowlist is matched
// against the hostname, not the host+port. An allowlisted host with an
// explicit port in the URL must pass CheckURL.
func TestEgress_HostWithExplicitPortPassesURLCheck(t *testing.T) {
	p := NewEgressPolicy([]string{"api.example.com"}, false)
	if err := p.CheckURL(mustParse(t, "https://api.example.com:8443/x")); err != nil {
		t.Fatalf("allowlisted host with explicit port must pass, got %v", err)
	}
	// A different host on the same port must still be denied.
	if err := p.CheckURL(mustParse(t, "https://evil.example.com:8443/x")); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("unlisted host with explicit port must be denied, got %v", err)
	}
}

// TestEgress_MaxRedirectsBlocked: exactly maxEgressRedirects hops is the
// cut-off — the (max+1)-th hop must be refused with ErrEgressBlocked.
func TestEgress_MaxRedirectsBlocked(t *testing.T) {
	// Build a chain of (maxEgressRedirects+1) servers, each redirecting to
	// the next. The last one returns 200. All hosts are allowlisted via the
	// loopback bypass.
	var servers []*httptest.Server
	const hops = maxEgressRedirects + 1
	// Allocate placeholder slice — we fill it back-to-front.
	servers = make([]*httptest.Server, hops)
	// Last server returns 200.
	servers[hops-1] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Each preceding server redirects to the next.
	for i := hops - 2; i >= 0; i-- {
		next := servers[i+1].URL
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, next, http.StatusFound)
		}))
	}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()

	// Collect all hostnames and allowlist them all.
	hosts := make([]string, hops)
	for i, s := range servers {
		hosts[i] = mustParse(t, s.URL).Hostname()
	}
	p := NewEgressPolicy(hosts, true).InsecureAllowPrivateForTest()
	resp, err := p.Client().Get(servers[0].URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("chain of %d redirects must be blocked at the limit, but succeeded", hops)
	}
	if !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("expected ErrEgressBlocked beyond redirect limit, got %v", err)
	}
}
