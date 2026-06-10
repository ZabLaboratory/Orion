package effects

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The `http.request` egress policy (ADR 003 §3.1.6 / risk R2-B1,
// Amendment 1 framing): the LANGUAGE is served in full — `http.request`
// always executes — but the EXECUTOR is bounded by a deployment
// policy. Fail-closed by default:
//
//   - empty allowlist ⇒ deny-all;
//   - https-only unless explicitly relaxed (ORION_HTTP_EGRESS_ALLOW_HTTP);
//   - private / loopback / link-local / metadata (169.254.169.254) /
//     unspecified / multicast destinations are denied at the DIAL, on
//     the IP RESOLVED AFTER DNS — the connection is made to the vetted
//     IP literal, so a DNS-rebinding flip between check and dial cannot
//     reach an internal address;
//   - redirects re-run the full URL policy on every hop.
//
// A denial is effect semantics: the blueprint's `error` output port
// fires (counted on orion_http_egress_blocked_total), never a crash,
// never a task kill.

// ErrEgressBlocked tags every policy denial so callers can count it.
var ErrEgressBlocked = errors.New("effects: egress blocked")

// EgressPolicy is the host/scheme allowlist + IP vetting. Immutable
// after construction.
type EgressPolicy struct {
	allowHosts map[string]struct{}
	allowHTTP  bool

	// lookup resolves a hostname — net.DefaultResolver in prod,
	// injectable so tests prove the post-DNS re-check (rebinding)
	// without real DNS.
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)

	// insecureAllowPrivate disables the resolved-IP vetting. NEVER
	// wired to configuration — set directly by tests that must dial a
	// loopback httptest server. The constructor always leaves it false.
	insecureAllowPrivate bool
}

// NewEgressPolicy builds the policy from the étage-1 configuration
// (ORION_HTTP_EGRESS_ALLOW_HOSTS, ORION_HTTP_EGRESS_ALLOW_HTTP).
// Hostnames are case-folded; an empty list is a valid deny-all policy.
func NewEgressPolicy(allowHosts []string, allowHTTP bool) *EgressPolicy {
	hosts := make(map[string]struct{}, len(allowHosts))
	for _, h := range allowHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			hosts[h] = struct{}{}
		}
	}
	return &EgressPolicy{
		allowHosts: hosts,
		allowHTTP:  allowHTTP,
		lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
			return net.DefaultResolver.LookupIPAddr(ctx, host)
		},
	}
}

// InsecureAllowPrivateForTest disables the resolved-IP vetting so
// tests can dial loopback httptest servers. TEST-ONLY by contract:
// no configuration path reaches it (the constructor cannot set it),
// and production code never calls it — the guard test in
// egress_test.go enumerates its callers' package.
func (p *EgressPolicy) InsecureAllowPrivateForTest() *EgressPolicy {
	p.insecureAllowPrivate = true
	return p
}

// SetLookupForTest injects the DNS resolver — tests prove the
// post-DNS re-check (DNS rebinding) without real DNS. TEST-ONLY.
func (p *EgressPolicy) SetLookupForTest(fn func(ctx context.Context, host string) ([]net.IPAddr, error)) {
	p.lookup = fn
}

// CheckURL applies the scheme + host-allowlist half of the policy
// (the resolved-IP half runs at dial time). Returns an
// ErrEgressBlocked-wrapped error on denial.
func (p *EgressPolicy) CheckURL(u *url.URL) error {
	switch u.Scheme {
	case "https":
	case "http":
		if !p.allowHTTP {
			return fmt.Errorf("%w: scheme %q (https-only policy)", ErrEgressBlocked, u.Scheme)
		}
	default:
		return fmt.Errorf("%w: scheme %q", ErrEgressBlocked, u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrEgressBlocked)
	}
	if _, ok := p.allowHosts[host]; !ok {
		return fmt.Errorf("%w: host %q not in allowlist", ErrEgressBlocked, host)
	}
	return nil
}

// blockedIP reports whether a resolved destination address is denied:
// loopback, RFC1918/ULA private, link-local (incl. the cloud metadata
// endpoint 169.254.169.254), unspecified and multicast ranges.
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// dialContext resolves the host, vets EVERY candidate IP, and dials
// the first allowed one BY ITS LITERAL — the post-DNS re-check that
// closes DNS rebinding (the hostname is never handed to the kernel
// resolver a second time).
func (p *EgressPolicy) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: address %q: %v", ErrEgressBlocked, addr, err)
	}
	var ips []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IPAddr{{IP: ip}}
	} else {
		ips, err = p.lookup(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	var lastErr error
	for _, ipa := range ips {
		if !p.insecureAllowPrivate && blockedIP(ipa.IP) {
			lastErr = fmt.Errorf("%w: host %q resolves to denied address %s", ErrEgressBlocked, host, ipa.IP)
			continue
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
		if err != nil {
			lastErr = err
			continue
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: host %q resolved to no address", ErrEgressBlocked, host)
	}
	return nil, lastErr
}

// maxEgressRedirects bounds redirect chains; each hop re-runs CheckURL.
const maxEgressRedirects = 5

// Client builds an *http.Client enforcing the full policy: vetting
// dialer + per-hop redirect re-check. The per-effect timeout is the
// job context's, not the client's.
func (p *EgressPolicy) Client() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: p.dialContext,
			// No proxy: a proxy would bypass the resolved-IP vetting.
			Proxy: nil,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxEgressRedirects {
				return fmt.Errorf("%w: too many redirects", ErrEgressBlocked)
			}
			return p.CheckURL(req.URL)
		},
	}
}
