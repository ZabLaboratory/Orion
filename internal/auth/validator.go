package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ValidationResult mirrors ZabAuth's
// `GET /auth/api/v1/tokens/{jti}/validate` response shape.
// Per the ZabAuth chantier brief: `{valid, role, paths, expires_at}`.
type ValidationResult struct {
	Valid     bool      `json:"valid"`
	Role      Role      `json:"role"`
	Paths     []string  `json:"paths"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Validator queries ZabAuth's /validate endpoint with a small
// in-memory cache. ZabGate already validated the token's signature on
// upgrade; this is the additional revocation/expiry probe Orion
// performs for show-tokens that came in via the WS query string.
type Validator struct {
	endpoint     string // e.g. http://zabgate:4000/auth/api/v1/tokens
	serviceToken string // Orion's own service token, presented as Bearer
	httpClient   *http.Client
	cacheTTL     time.Duration

	mu    sync.Mutex
	cache map[string]cachedValidation
	now   func() time.Time // injected for tests
}

type cachedValidation struct {
	result    ValidationResult
	expiresAt time.Time // local cache expiry — distinct from ValidationResult.ExpiresAt
}

// NewValidator builds a validator. endpoint should NOT include the
// `/{jti}/validate` suffix — Validator appends it per call.
func NewValidator(endpoint, serviceToken string, cacheTTL time.Duration) *Validator {
	return &Validator{
		endpoint:     strings.TrimRight(endpoint, "/"),
		serviceToken: serviceToken,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		cacheTTL:     cacheTTL,
		cache:        make(map[string]cachedValidation),
		now:          time.Now,
	}
}

// Validate calls /validate, caching successful results for cacheTTL.
// A failed validation is cached with a much shorter TTL (2 s) so a
// flapping ZabAuth doesn't either pin "invalid" forever or get
// hammered.
func (v *Validator) Validate(ctx context.Context, jti string) (ValidationResult, error) {
	if jti == "" {
		return ValidationResult{}, errors.New("auth: empty jti")
	}

	now := v.now()

	v.mu.Lock()
	if hit, ok := v.cache[jti]; ok && hit.expiresAt.After(now) {
		v.mu.Unlock()
		return hit.result, nil
	}
	v.mu.Unlock()

	res, err := v.fetch(ctx, jti)
	if err != nil {
		return ValidationResult{}, err
	}

	ttl := v.cacheTTL
	if !res.Valid {
		// Short cache on negative answers so a fresh issuance picks up quickly.
		ttl = 2 * time.Second
	}
	if ttl > 0 {
		v.mu.Lock()
		v.cache[jti] = cachedValidation{result: res, expiresAt: now.Add(ttl)}
		v.mu.Unlock()
	}
	return res, nil
}

func (v *Validator) fetch(ctx context.Context, jti string) (ValidationResult, error) {
	url := fmt.Sprintf("%s/%s/validate", v.endpoint, jti)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ValidationResult{}, err
	}
	if v.serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+v.serviceToken)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("auth: validate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ValidationResult{Valid: false}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return ValidationResult{}, fmt.Errorf("auth: validate: status %d", resp.StatusCode)
	}

	var out ValidationResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ValidationResult{}, fmt.Errorf("auth: decode validate: %w", err)
	}
	return out, nil
}
