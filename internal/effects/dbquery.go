package effects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// `db.query` topology A (ADR 003 Amendment 1, wire contract §1):
// Orion holds NO DB credential, NO pgx pool, emits NO SQL. It POSTs
// the authored QueryDescriptor to `${ZABGATE_URL}/<svc>/api/v1/_query`
// with its own service token; the service owning the DataSource
// compiles (QueryMe — read-only by construction, parameterized) and
// executes against ITS database. The token must carry the per-service
// scope `query.read.<svc>` (enforced by the owning service, P2).

// DataSource is one declared logical source: the authored name and the
// ZabGate prefix of the owning service.
type DataSource struct {
	Name string
	Svc  string
}

// Scope is the service-token scope the owning service asserts
// (wire contract §1.3, option A — per-service).
func (d DataSource) Scope() string { return "query.read." + d.Svc }

// ParseDataSources parses the ORION_DATASOURCES étage-1 CSV
// (`<logical_name>=<zabgate_svc>,...`, e.g. `truth=truth,ranking=ranking`).
// Empty input is a valid empty allowlist (every db.query node then
// fails compile with DATASOURCE_NOT_DECLARED — structural validation,
// never a capability refusal: db.query stays fully served).
func ParseDataSources(raw string) (map[string]DataSource, error) {
	out := map[string]DataSource{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, svc, ok := strings.Cut(part, "=")
		name, svc = strings.TrimSpace(name), strings.TrimSpace(svc)
		if !ok || name == "" || svc == "" {
			return nil, fmt.Errorf("effects: ORION_DATASOURCES entry %q is not <name>=<svc>", part)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("effects: ORION_DATASOURCES duplicate name %q", name)
		}
		out[name] = DataSource{Name: name, Svc: svc}
	}
	return out, nil
}

// QueryResult is the `_query` 200 response shape (wire contract §1.1).
type QueryResult struct {
	Rows      json.RawMessage `json:"rows"`
	Count     int             `json:"count"`
	ElapsedMS float64         `json:"elapsed_ms"`
}

// DBQueryClient calls `_query` through ZabGate with Orion's service
// token. It deliberately has no egress policy: the gateway URL is
// operator deployment config, never a blueprint-authored destination.
type DBQueryClient struct {
	gatewayURL string // e.g. http://zabgate:4000 (no trailing slash)
	// tokenFn returns the CURRENT service token on every call so a
	// rotation by the ServiceTokenManager is reflected immediately. A
	// frozen boot token would 401 every `_query` once it rotated — the
	// same C1 bug class the compiler fetcher hit
	// (NewHTTPFetcherWithTokenFunc).
	tokenFn     func() string
	pathTokenFn func([]string) string
	client      *http.Client
}

// NewDBQueryClient builds the client with a STATIC token. Kept for tests
// and static-mode callers. httpClient nil = http.DefaultClient.
func NewDBQueryClient(gatewayURL, serviceToken string, httpClient *http.Client) *DBQueryClient {
	return NewDBQueryClientWithTokenFunc(gatewayURL, func() string { return serviceToken }, httpClient)
}

// NewDBQueryClientWithTokenFunc builds the client reading its bearer LIVE
// from tokenFn on every request (prod wiring: tokenFn =
// ServiceTokenManager.Token). tokenFn nil = no Authorization header.
// httpClient nil = http.DefaultClient.
func NewDBQueryClientWithTokenFunc(gatewayURL string, tokenFn func() string, httpClient *http.Client) *DBQueryClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if tokenFn == nil {
		tokenFn = func() string { return "" }
	}
	return &DBQueryClient{
		gatewayURL: strings.TrimRight(gatewayURL, "/"),
		tokenFn:    tokenFn,
		client:     httpClient,
	}
}

// NewDBQueryClientWithPathTokenFunc asks the minter for the exact datasource
// scope on every query. It is the production Engine B constructor; the
// parameterless constructor remains for legacy/static tests.
func NewDBQueryClientWithPathTokenFunc(gatewayURL string, tokenFn func([]string) string, httpClient *http.Client) *DBQueryClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if tokenFn == nil {
		tokenFn = func([]string) string { return "" }
	}
	return &DBQueryClient{
		gatewayURL:  strings.TrimRight(gatewayURL, "/"),
		pathTokenFn: tokenFn,
		client:      httpClient,
	}
}

// maxQueryResponse bounds the `_query` response read (same 1 MiB bound
// as the poller).
const maxQueryResponse = 1 << 20

// Query POSTs the descriptor to the owning service. A non-200 response
// is returned as an error string suitable for the effect's `error`
// port (400 carries the service's structured issues verbatim).
func (c *DBQueryClient) Query(ctx context.Context, ds DataSource, descriptor json.RawMessage) (*QueryResult, error) {
	u := fmt.Sprintf("%s/%s/api/v1/_query", c.gatewayURL, ds.Svc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(descriptor))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	tok := ""
	if c.pathTokenFn != nil {
		tok = c.pathTokenFn([]string{ds.Scope()})
	} else if c.tokenFn != nil {
		tok = c.tokenFn()
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQueryResponse))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("_query %s: status %d: %s", ds.Name, resp.StatusCode, body)
	}
	var out QueryResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("_query %s: malformed response: %w", ds.Name, err)
	}
	return &out, nil
}
