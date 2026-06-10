package effects

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Tests for the http.request egress policy (ADR 003 §3.1.6, R2/B1,
// issue #85): fail-closed allowlist, https-only default, and the
// post-DNS resolved-IP vetting that closes DNS rebinding.

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestEgress_EmptyAllowlistDeniesAll: the unconfigured policy is
// deny-all — fail-closed by default.
func TestEgress_EmptyAllowlistDeniesAll(t *testing.T) {
	p := NewEgressPolicy(nil, false)
	if err := p.CheckURL(mustParse(t, "https://api.example.com/x")); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("empty allowlist must deny-all, got %v", err)
	}
}

// TestEgress_SchemePolicy: https-only by default; http only when
// explicitly relaxed; every other scheme always denied.
func TestEgress_SchemePolicy(t *testing.T) {
	p := NewEgressPolicy([]string{"api.example.com"}, false)
	if err := p.CheckURL(mustParse(t, "https://api.example.com/x")); err != nil {
		t.Fatalf("allowlisted https must pass: %v", err)
	}
	if err := p.CheckURL(mustParse(t, "http://api.example.com/x")); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("http must be denied under the https-only default, got %v", err)
	}
	if err := p.CheckURL(mustParse(t, "ftp://api.example.com/x")); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("non-http(s) scheme must be denied, got %v", err)
	}
	relaxed := NewEgressPolicy([]string{"api.example.com"}, true)
	if err := relaxed.CheckURL(mustParse(t, "http://api.example.com/x")); err != nil {
		t.Fatalf("http must pass when explicitly relaxed: %v", err)
	}
}

// TestEgress_HostAllowlist: only listed hosts pass, case-folded; a
// subdomain of a listed host is NOT implicitly allowed.
func TestEgress_HostAllowlist(t *testing.T) {
	p := NewEgressPolicy([]string{"API.Example.com"}, false)
	if err := p.CheckURL(mustParse(t, "https://api.example.com/x")); err != nil {
		t.Fatalf("case-folded allowlisted host must pass: %v", err)
	}
	if err := p.CheckURL(mustParse(t, "https://evil.example.com/x")); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("unlisted host must be denied, got %v", err)
	}
	if err := p.CheckURL(mustParse(t, "https://sub.api.example.com/x")); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("subdomain must not inherit the allowlist entry, got %v", err)
	}
}

// TestEgress_BlockedIPRanges: the resolved-address vetting denies
// loopback, RFC1918, link-local (incl. the 169.254.169.254 metadata
// endpoint), ULA, unspecified and multicast.
func TestEgress_BlockedIPRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1",
		"10.0.0.1", "172.16.5.5", "192.168.1.1",
		"169.254.169.254", "169.254.0.1", "fe80::1",
		"fd00::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
	}
	for _, s := range blocked {
		if !blockedIP(net.ParseIP(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	allowed := []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "8.8.8.8"}
	for _, s := range allowed {
		if blockedIP(net.ParseIP(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

// TestEgress_PostDNSRebindingBlocked: the anti-SSRF check runs on the
// IP RESOLVED AFTER DNS, not the hostname — an allowlisted hostname
// that (re)binds to an internal address is denied at dial.
func TestEgress_PostDNSRebindingBlocked(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.7", "169.254.169.254"} {
		p := NewEgressPolicy([]string{"rebind.example.com"}, false)
		p.SetLookupForTest(func(_ context.Context, _ string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		})
		// The URL half passes (the hostname IS allowlisted)…
		if err := p.CheckURL(mustParse(t, "https://rebind.example.com/x")); err != nil {
			t.Fatalf("URL check should pass for the allowlisted name: %v", err)
		}
		// …the dial half must still deny the resolved internal IP.
		_, err := p.dialContext(context.Background(), "tcp", "rebind.example.com:443")
		if !errors.Is(err, ErrEgressBlocked) {
			t.Fatalf("resolved %s must be denied post-DNS, got %v", ip, err)
		}
	}
}

// TestEgress_IPLiteralBlockedAtDial: an IP-literal URL never reaches
// DNS — the dialer vets the literal directly.
func TestEgress_IPLiteralBlockedAtDial(t *testing.T) {
	p := NewEgressPolicy([]string{"169.254.169.254"}, false)
	_, err := p.dialContext(context.Background(), "tcp", "169.254.169.254:443")
	if !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("metadata IP literal must be denied, got %v", err)
	}
}

// TestEgress_RedirectRecheck: every redirect hop re-runs the URL
// policy — an allowlisted origin cannot bounce the client to an
// unlisted host.
func TestEgress_RedirectRecheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://internal.example.com/secret", http.StatusFound)
	}))
	defer srv.Close()

	host := mustParse(t, srv.URL).Hostname()
	p := NewEgressPolicy([]string{host}, true).InsecureAllowPrivateForTest()
	resp, err := p.Client().Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("redirect to unlisted host must fail")
	}
	if !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("expected ErrEgressBlocked on redirect, got %v", err)
	}
}
