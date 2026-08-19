package effects

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ServiceTokenExchangeMinter turns one operator-provisioned durable family
// access token into exact, short-lived route tokens through ZabAuth. The
// family token is never sent to a downstream data/service route.
type ServiceTokenExchangeMinter struct {
	url           string
	familyTokenFn func() string
	client        *http.Client
	mu            sync.Mutex
	cache         map[string]cachedExchange
}

type cachedExchange struct {
	token     string
	expiresAt time.Time
}

type exchangeRequest struct {
	Paths []string `json:"paths"`
	TTLS  int      `json:"ttl_s"`
}

type exchangeResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// NewServiceTokenExchangeMinter builds the production callback. An empty
// family token deliberately leaves the callback fail-closed.
func NewServiceTokenExchangeMinter(gatewayURL, familyToken string, client *http.Client) *ServiceTokenExchangeMinter {
	return NewServiceTokenExchangeMinterWithTokenFunc(gatewayURL, func() string { return familyToken }, client)
}

// NewServiceTokenExchangeMinterWithTokenFunc reads the current family token
// at exchange time. This is required after a durable rotation: freezing the
// token at Orion boot turns the first ZabAuth rotation into a delayed 401.
func NewServiceTokenExchangeMinterWithTokenFunc(gatewayURL string, familyTokenFn func() string, client *http.Client) *ServiceTokenExchangeMinter {
	if client == nil {
		client = http.DefaultClient
	}
	if familyTokenFn == nil {
		familyTokenFn = func() string { return "" }
	}
	return &ServiceTokenExchangeMinter{
		url:           strings.TrimRight(gatewayURL, "/") + "/auth/api/v1/service-tokens/exchange",
		familyTokenFn: familyTokenFn,
		client:        client,
		cache:         map[string]cachedExchange{},
	}
}

func exchangeKey(paths []string) (string, []string) {
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			set[path] = struct{}{}
		}
	}
	normalized := make([]string, 0, len(set))
	for path := range set {
		normalized = append(normalized, path)
	}
	sort.Strings(normalized)
	return strings.Join(normalized, "\x00"), normalized
}

// Token returns a cached or freshly exchanged bearer for exactly paths. It
// returns "" on any exchange failure so callers fail closed on their error
// port without exposing credential details to the blueprint runtime.
func (m *ServiceTokenExchangeMinter) Token(paths []string) string {
	if m == nil || m.familyTokenFn == nil {
		return ""
	}
	familyToken := m.familyTokenFn()
	if familyToken == "" {
		return ""
	}
	key, normalized := exchangeKey(paths)
	if key == "" {
		return ""
	}
	now := time.Now()
	m.mu.Lock()
	if cached, ok := m.cache[key]; ok && cached.token != "" && now.Before(cached.expiresAt.Add(-5*time.Second)) {
		m.mu.Unlock()
		return cached.token
	}
	m.mu.Unlock()

	body, err := json.Marshal(exchangeRequest{Paths: normalized, TTLS: 300})
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url, bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+familyToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var exchanged exchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&exchanged); err != nil || exchanged.AccessToken == "" || exchanged.ExpiresAt.IsZero() {
		return ""
	}
	if !exchanged.ExpiresAt.After(now) {
		return ""
	}
	m.mu.Lock()
	m.cache[key] = cachedExchange{token: exchanged.AccessToken, expiresAt: exchanged.ExpiresAt}
	m.mu.Unlock()
	return exchanged.AccessToken
}
