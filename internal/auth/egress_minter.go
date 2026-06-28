package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EgressTokenSource mints + caches service tokens scoped to a SPECIFIC set
// of paths, for the curated service-egress node (`core.service.call@1`,
// ADR Blue 002 §3.3). Unlike ServiceTokenManager — which holds ONE token
// for Orion's fixed surface — each curated route declares its own
// token_paths, so this source mints a per-path-set token and caches it,
// re-minting before expiry.
//
// Security posture (ADR 002 §5, surface flagged for Bastion):
//   - the token is minted with EXACTLY the route's token_paths (no union,
//     no widening) — the tightest scope, the assertion RC #4 checks;
//   - a mint failure / missing operator token returns "" — FAIL-CLOSED:
//     the call resolves to its `error` port, never an anonymous request;
//   - neither the operator token nor any minted token is ever logged
//     (counts + path-set only).
//
// Static mode (empty OperatorToken) returns "" for every request: a dev /
// test deployment without operator credentials cannot egress (fail-closed),
// which is the intended posture until the operator token is wired.
type EgressTokenSource struct {
	// MintURL is ZabAuth's /service-tokens endpoint via ZabGate.
	MintURL string
	// OperatorToken is the admin JWT presented to mint. Empty → no egress.
	OperatorToken string
	// ServiceName sent in the mint body (ZabAuth audits it).
	ServiceName string
	// MintTTL requested at mint (ZabAuth caps at 1 h). Default 1 h.
	MintTTL time.Duration
	// RefreshLead re-mints this long before expiry. Default 5 min.
	RefreshLead time.Duration
	// HTTPClient reused for mint; default 10 s timeout.
	HTTPClient *http.Client
	Logger     *slog.Logger

	mu    sync.Mutex
	cache map[string]egressBundle
}

type egressBundle struct {
	token     string
	expiresAt time.Time
}

// pathsKey is the cache key for a token-paths set — order-independent so
// the same scope reuses one cached token.
func pathsKey(paths []string) string {
	cp := append([]string(nil), paths...)
	sort.Strings(cp)
	return strings.Join(cp, "\x00")
}

// Token returns a bearer scoped to EXACTLY paths, minting (and caching) on
// first use or when the cached one is within RefreshLead of expiry. Returns
// "" fail-closed on any failure or in static mode.
func (s *EgressTokenSource) Token(paths []string) string {
	if s.OperatorToken == "" || len(paths) == 0 {
		return ""
	}
	lead := s.RefreshLead
	if lead <= 0 {
		lead = 5 * time.Minute
	}
	key := pathsKey(paths)

	s.mu.Lock()
	if b, ok := s.cache[key]; ok && time.Until(b.expiresAt) > lead {
		tok := b.token
		s.mu.Unlock()
		return tok
	}
	s.mu.Unlock()

	bundle, err := s.mint(context.Background(), paths)
	if err != nil {
		if s.Logger != nil {
			// Never log the token or the operator credential — type + scope only.
			s.Logger.Warn("egress token mint failed", "paths", len(paths), "err", err)
		}
		return ""
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]egressBundle{}
	}
	s.cache[key] = bundle
	s.mu.Unlock()
	return bundle.token
}

func (s *EgressTokenSource) mint(ctx context.Context, paths []string) (egressBundle, error) {
	ttl := s.MintTTL
	if ttl <= 0 {
		ttl = 60 * time.Minute
	}
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	body, _ := json.Marshal(map[string]any{
		"service": s.ServiceName,
		"paths":   paths,
		"ttl_s":   int(ttl.Seconds()),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.MintURL, bytes.NewReader(body))
	if err != nil {
		return egressBundle{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.OperatorToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return egressBundle{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return egressBundle{}, &mintStatusError{status: resp.StatusCode}
	}
	var out ServiceTokenBundle
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return egressBundle{}, err
	}
	return egressBundle{token: out.AccessToken, expiresAt: out.ExpiresAt}, nil
}

type mintStatusError struct{ status int }

func (e *mintStatusError) Error() string {
	return "egress mint: zabauth status " + strconv.Itoa(e.status)
}
