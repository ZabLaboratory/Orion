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

// Curated service-egress (`core.service.call@1`, ADR Blue 002). UNLIKE
// `http.request` this client carries NO authored URL: the gateway base is
// operator deployment config and the path is built from the curated
// route's path_template (the runtime escapes the params — egress.go on the
// runtime side). It POSTs through ZabGate with a service token minted for
// the route's token_paths ONLY, and it NEVER forwards the caller's
// Authorization — the identity is always `service:orion`. This is what
// closes the confused-deputy ADR 002 §3.6.A rejected for http.request.

// ServiceCallResult is the parsed `service.call` response surface bound on
// the node's `then` pin (status/ok/body), mirroring `http.request`.
type ServiceCallResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// TokenMinter returns a bearer scoped to EXACTLY the given token paths, or
// "" when no scoped token is available (fail-closed — the call then errors
// on its `error` port, never an anonymous request). In prod this is backed
// by a ServiceTokenManager-style minter; tests inject a recording stub to
// assert the paths a route is minted against (ADR 002 RC #4).
type TokenMinter func(paths []string) string

// ServiceCallClient issues curated egress calls through the gateway.
// Like DBQueryClient it has NO egress (anti-SSRF) policy: the gateway URL
// is operator config, never an authored destination.
type ServiceCallClient struct {
	gatewayURL string
	mint       TokenMinter
	client     *http.Client
}

// NewServiceCallClient builds the client. mint nil = no token (every call
// fail-closed to its `error` port). httpClient nil = http.DefaultClient.
func NewServiceCallClient(gatewayURL string, mint TokenMinter, httpClient *http.Client) *ServiceCallClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if mint == nil {
		mint = func([]string) string { return "" }
	}
	return &ServiceCallClient{
		gatewayURL: strings.TrimRight(gatewayURL, "/"),
		mint:       mint,
		client:     httpClient,
	}
}

// maxServiceCallResponse bounds the response read (same 1 MiB cap as the
// other effects).
const maxServiceCallResponse = 1 << 20

// escapeSegment percent-encodes every character outside the RFC 3986
// unreserved set (ALPHA / DIGIT / -._~) — byte-for-byte parity with Blue's
// build_path (`urllib.parse.quote(value, safe="")`). This guarantees a
// param value can never add a path segment (`/` → %2F), traverse (the dots
// stay but the separating slash is encoded), or smuggle a query (`?`/`&`/
// `=` all encoded).
func escapeSegment(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}

// BuildPath fills a curated path_template from its params, escaping every
// value (escapeSegment) so an authored param can never add a segment,
// traverse with `..`, or smuggle a query string — the only structural
// slashes are the template's own. A param the template requires but the
// call omits is a hard error (the authoring validator catches it; this is
// the runtime backstop). The template is CURATED data (baked at compile
// from the registry), never the authored graph.
func BuildPath(pathTemplate string, params []string, values map[string]string) (string, error) {
	out := pathTemplate
	for _, name := range params {
		v, ok := values[name]
		if !ok {
			return "", fmt.Errorf("missing template param %q", name)
		}
		out = strings.ReplaceAll(out, "{"+name+"}", escapeSegment(v))
	}
	return out, nil
}

// Call performs the curated egress: build the absolute URL from the
// gateway base + the runtime-constructed path, mint a token scoped to
// tokenPaths, and POST/PUT/… the payload. A non-2xx is returned verbatim
// in the result (status + body) so the author can branch on it; a
// transport failure or a missing scoped token is an error.
func (c *ServiceCallClient) Call(
	ctx context.Context,
	method, path string,
	tokenPaths []string,
	payload json.RawMessage,
) (*ServiceCallResult, error) {
	token := c.mint(tokenPaths)
	if token == "" {
		// Fail-closed (ADR 002 §5 Q3): no scoped token ⇒ no anonymous
		// request. The node resolves to its `error` port.
		return nil, fmt.Errorf("no scoped egress token for %v", tokenPaths)
	}
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	u := c.gatewayURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// The identity is always service:orion — the caller's Authorization is
	// NEVER forwarded (ADR 002 §3.3). Only this scoped service token rides.
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxServiceCallResponse))
	if err != nil {
		return nil, err
	}
	out := &ServiceCallResult{Status: resp.StatusCode}
	switch {
	case len(body) == 0:
		out.Body = json.RawMessage("null")
	case json.Valid(body):
		out.Body = json.RawMessage(body)
	default:
		// Non-JSON body: wrap as a JSON string so the `body` pin stays json.
		s, _ := json.Marshal(string(body))
		out.Body = s
	}
	return out, nil
}
