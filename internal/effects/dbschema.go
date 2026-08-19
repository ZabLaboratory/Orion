package effects

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DB schema discovery (ADR Blue 008 §3.4). Strictly READ-ONLY: Orion
// proxies the owning service's `GET /<svc>/api/v1/_schema` — the SAME
// static catalog that the service's query validator whitelists tables
// and columns against (QueryMe `validate_against_schema`). This adds NO
// access beyond the read-schema surface already exposed to Orion's
// service token: no query is executed, no column outside the catalog is
// reachable. The catalog IS the implicit whitelist of `core.db.query@1`.

// SchemaColumn is one column in a service catalog (subset of QueryMe
// ColumnDef — only the fields the DB catalog surfaces).
type SchemaColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Primary  bool   `json:"primary"`
}

// SchemaTable is one whitelisted table.
type SchemaTable struct {
	Name    string         `json:"name"`
	Columns []SchemaColumn `json:"columns"`
}

// Schema is the `_schema` 200 response shape (QueryMe SchemaDescriptor,
// reduced to the fields the catalog needs). Unknown fields are ignored.
type Schema struct {
	Service string        `json:"service"`
	Tables  []SchemaTable `json:"tables"`
}

// SchemaClient fetches a service catalog through ZabGate with Orion's
// service token — same wiring as DBQueryClient (no egress policy: the
// gateway URL is operator config, never blueprint-authored).
type SchemaClient struct {
	gatewayURL  string
	tokenFn     func() string
	pathTokenFn func([]string) string
	client      *http.Client
}

// NewSchemaClientWithTokenFunc builds the client reading its bearer LIVE
// from tokenFn on every request (prod wiring: tokenFn =
// ServiceTokenManager.Token). tokenFn nil = no Authorization header.
// httpClient nil = http.DefaultClient.
func NewSchemaClientWithTokenFunc(gatewayURL string, tokenFn func() string, httpClient *http.Client) *SchemaClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if tokenFn == nil {
		tokenFn = func() string { return "" }
	}
	return &SchemaClient{
		gatewayURL: strings.TrimRight(gatewayURL, "/"),
		tokenFn:    tokenFn,
		client:     httpClient,
	}
}

// NewSchemaClientWithPathTokenFunc asks the minter for the exact datasource
// read scope on every catalog request.
func NewSchemaClientWithPathTokenFunc(gatewayURL string, tokenFn func([]string) string, httpClient *http.Client) *SchemaClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if tokenFn == nil {
		tokenFn = func([]string) string { return "" }
	}
	return &SchemaClient{
		gatewayURL:  strings.TrimRight(gatewayURL, "/"),
		pathTokenFn: tokenFn,
		client:      httpClient,
	}
}

// Schema GETs the catalog of the owning service. A non-200 is returned
// as an error suitable for surfacing upstream.
func (c *SchemaClient) Schema(ctx context.Context, ds DataSource) (*Schema, error) {
	u := fmt.Sprintf("%s/%s/api/v1/_schema", c.gatewayURL, ds.Svc)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("_schema %s: status %d: %s", ds.Name, resp.StatusCode, body)
	}
	var out Schema
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("_schema %s: malformed response: %w", ds.Name, err)
	}
	return &out, nil
}

// SearchableColumns are the columns a cockpit selector can search on:
// non-primary string/text columns. Primary keys (opaque UUIDs) and
// numeric/temporal columns are not free-text searchable. Order follows
// the catalog declaration (deterministic).
func (t SchemaTable) SearchableColumns() []string {
	out := []string{}
	for _, c := range t.Columns {
		if c.Primary {
			continue
		}
		if c.Type == "string" || c.Type == "text" {
			out = append(out, c.Name)
		}
	}
	return out
}

// ResultShape is the row shape the table yields: column name → type for
// every whitelisted column. Drives the cockpit's preview of a selection.
func (t SchemaTable) ResultShape() map[string]string {
	out := make(map[string]string, len(t.Columns))
	for _, c := range t.Columns {
		out[c.Name] = c.Type
	}
	return out
}
