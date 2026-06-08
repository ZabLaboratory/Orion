package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ServiceTokenBundle mirrors ZabAuth's /service-tokens response shape.
// Source: ZabAuth/src/zabauth/schemas/tokens.py::TokenIssueResponse.
type ServiceTokenBundle struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

// ServiceTokenManager mints and rotates Orion's outbound service token
// against ZabAuth via ZabGate. Mirror of Quasar's Python implementation
// in quasar/core/orion_client.py — same ZabAuth contract, same refresh
// strategy: wake N seconds before expiry, rotate, swap in place under
// a write lock.
//
// Two operating modes:
//
//   - **Live mode** — OperatorToken is set. Manager mints a service
//     token at Start(), then runs a background goroutine that refreshes
//     it RefreshLead before expiry. Token() returns the live access
//     token.
//   - **Static mode** — OperatorToken is empty. Manager falls back to
//     the static StaticToken (ORION_SERVICE_TOKEN env var). No mint, no
//     refresh loop. This is the pre-Quasar wiring posture and lets
//     dev / tests skip the ZabAuth round-trip.
type ServiceTokenManager struct {
	// MintURL is ZabAuth's /service-tokens endpoint via ZabGate.
	// e.g., http://zabgate:4000/auth/api/v1/service-tokens
	MintURL string
	// RefreshURL is ZabAuth's /service-tokens/refresh endpoint via
	// ZabGate. Trailing /refresh appended at construction.
	RefreshURL string
	// OperatorToken is an admin JWT used once at boot to mint the
	// service token. Empty → static mode.
	OperatorToken string
	// StaticToken is the fallback used when OperatorToken is empty.
	// Mirrors today's `cfg.ServiceToken` direct usage.
	StaticToken string
	// ServiceName sent in the mint request body. ZabAuth audits it.
	ServiceName string
	// Paths claim — restricts the token's WS write scope. For HTTP
	// outbound calls (the credentials proxy) the paths claim is
	// documentation; ZabGate enforces only role+validity at the
	// upgrade. Per chantier brief, scope it to the Quasar surface.
	Paths []string
	// RefreshLead is how long before expiry the manager rotates.
	// Default 5 min — same as Quasar.
	RefreshLead time.Duration
	// MintTTL is the TTL requested when minting. ZabAuth caps at 1 h.
	MintTTL time.Duration
	// HTTPClient is reused for mint + refresh. Defaults to a 10 s
	// timeout when nil.
	HTTPClient *http.Client
	// Logger — required in live mode.
	Logger *slog.Logger

	mu     sync.RWMutex
	bundle ServiceTokenBundle

	stop    chan struct{}
	stopped chan struct{}
	live    bool
}

// Token returns the current access token.
//
// In static mode it returns StaticToken unchanged.
// In live mode it returns the latest minted access token (concurrency-
// safe ; readers see a fully-rotated value, never a half-set struct).
func (m *ServiceTokenManager) Token() string {
	if !m.live {
		return m.StaticToken
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bundle.AccessToken
}

// Start mints the initial token (live mode only) and launches the
// refresh loop. Idempotent: calling Start a second time is a no-op.
//
// In static mode, Start is a no-op — Token() returns StaticToken.
func (m *ServiceTokenManager) Start(ctx context.Context) error {
	if m.OperatorToken == "" {
		return nil
	}
	if m.stop != nil {
		return nil
	}
	if m.RefreshLead <= 0 {
		m.RefreshLead = 5 * time.Minute
	}
	if m.MintTTL <= 0 {
		m.MintTTL = 60 * time.Minute
	}
	if m.HTTPClient == nil {
		m.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}

	bundle, err := m.mint(ctx)
	if err != nil {
		return fmt.Errorf("service token: initial mint: %w", err)
	}
	m.mu.Lock()
	m.bundle = bundle
	m.mu.Unlock()
	m.live = true
	m.stop = make(chan struct{})
	m.stopped = make(chan struct{})
	go m.refreshLoop()
	return nil
}

// Stop signals the refresh goroutine and waits for it to exit. Safe to
// call from multiple goroutines ; safe to call when Start was never
// called or when in static mode.
func (m *ServiceTokenManager) Stop() {
	if m.stop == nil {
		return
	}
	close(m.stop)
	<-m.stopped
	m.stop = nil
	m.stopped = nil
	m.live = false
}

func (m *ServiceTokenManager) refreshLoop() {
	defer close(m.stopped)
	for {
		m.mu.RLock()
		expiresAt := m.bundle.ExpiresAt
		m.mu.RUnlock()
		wait := time.Until(expiresAt) - m.RefreshLead
		if wait < time.Second {
			wait = time.Second
		}
		select {
		case <-m.stop:
			return
		case <-time.After(wait):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		bundle, err := m.refresh(ctx)
		cancel()
		if err != nil {
			if m.Logger != nil {
				m.Logger.Warn("service token refresh failed; will retry", "err", err)
			}
			// Bound retry after a refresh failure ; we don't want a
			// hot loop if ZabAuth is down. 30 s is short enough that
			// a recovery is picked up promptly without saturating
			// either side.
			select {
			case <-m.stop:
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}
		m.mu.Lock()
		m.bundle = bundle
		m.mu.Unlock()
		if m.Logger != nil {
			m.Logger.Info("service token rotated", "expires_at", bundle.ExpiresAt)
		}
	}
}

func (m *ServiceTokenManager) mint(ctx context.Context) (ServiceTokenBundle, error) {
	body, _ := json.Marshal(map[string]any{
		"service": m.ServiceName,
		"paths":   m.Paths,
		"ttl_s":   int(m.MintTTL.Seconds()),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.MintURL, bytes.NewReader(body))
	if err != nil {
		return ServiceTokenBundle{}, err
	}
	req.Header.Set("Authorization", "Bearer "+m.OperatorToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return m.do(req, http.StatusCreated, http.StatusOK)
}

func (m *ServiceTokenManager) refresh(ctx context.Context) (ServiceTokenBundle, error) {
	m.mu.RLock()
	rt := m.bundle.RefreshToken
	m.mu.RUnlock()
	if rt == "" {
		return ServiceTokenBundle{}, errors.New("service token: refresh: no refresh token in bundle")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": rt})
	url := strings.TrimRight(m.RefreshURL, "/")
	if !strings.HasSuffix(url, "/refresh") {
		url = m.MintURL + "/refresh"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ServiceTokenBundle{}, err
	}
	// Bastion C3: ZabAuth authenticates the refresh caller by the same
	// operator Bearer mint() presents (mirror of mint, l.196) — the
	// refresh_token is the rotation credential and travels in the body,
	// it is NOT an Authorization bearer. Before this, refresh() set no
	// Authorization header at all, so ZabAuth 401'd every rotation and
	// the token silently went stale. The operator token (never the
	// refresh/access token) is the bearer; neither is ever logged (C4).
	req.Header.Set("Authorization", "Bearer "+m.OperatorToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return m.do(req, http.StatusOK)
}

func (m *ServiceTokenManager) do(req *http.Request, accept ...int) (ServiceTokenBundle, error) {
	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return ServiceTokenBundle{}, err
	}
	defer resp.Body.Close()
	ok := false
	for _, code := range accept {
		if resp.StatusCode == code {
			ok = true
			break
		}
	}
	if !ok {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ServiceTokenBundle{}, fmt.Errorf("zabauth status %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	var out ServiceTokenBundle
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ServiceTokenBundle{}, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}
