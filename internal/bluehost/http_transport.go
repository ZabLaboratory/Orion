package bluehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZabLaboratory/Orion/internal/effects"
)

// The direct EffectHandlers and the generic core.effect.invoke@1 adapter
// have different ABI envelopes, but they deliberately share this transport
// implementation. Keeping URL validation, request caps, header filtering,
// egress checks, and response decoding here prevents the two host surfaces
// from drifting while their protocol adapters remain separate.
const (
	maxHTTPTransportResponse = 1 << 20
	maxHTTPTransportHeaders  = 16 << 10
	maxHTTPTransportQuery    = 8 << 10
	maxHTTPTransportBody     = 1 << 20

	defaultHTTPTransportTimeout = 10 * time.Second
	maxHTTPTransportTimeout     = 60 * time.Second
)

var forbiddenHTTPTransportHeaders = map[string]struct{}{
	"authorization":       {},
	"cookie":              {},
	"proxy-authorization": {},
	"host":                {},
	"content-length":      {},
	"connection":          {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"te":                  {},
	"trailer":             {},
}

type httpTransportRequest struct {
	URL     string
	Method  string
	Query   any
	Headers any
	Body    []byte
}

type httpTransportResponse struct {
	Status  int
	Body    any
	Headers map[string]any
}

func (r httpTransportResponse) values() map[string]any {
	return map[string]any{
		"status":  json.Number(strconv.Itoa(r.Status)),
		"body":    r.Body,
		"headers": r.Headers,
	}
}

// executeHTTP is the single bluehost HTTP transport. Its callers adapt
// either the synchronous EffectHandlers inputs or the generic invocation
// request object into httpTransportRequest, then retain their own ABI and
// continuation semantics around this result.
func executeHTTP(ctx context.Context, egress *effects.EgressPolicy, request httpTransportRequest) (httpTransportResponse, error) {
	if egress == nil {
		return httpTransportResponse{}, errors.New("EFFECT_PROVIDER_UNAVAILABLE: no egress policy configured (deny-all)")
	}

	method := strings.ToUpper(request.Method)
	if method == "" {
		method = http.MethodGet
	}
	u, err := url.Parse(request.URL)
	if err != nil {
		// Never echo a malformed authored URL: it may carry a secret query.
		return httpTransportResponse{}, errors.New("HTTP_REQUEST_INVALID_URL: malformed url")
	}
	if len(request.Body) > maxHTTPTransportBody {
		return httpTransportResponse{}, errors.New("HTTP_REQUEST_BODY_TOO_LARGE")
	}
	if err := mergeHTTPTransportQuery(u, request.Query); err != nil {
		return httpTransportResponse{}, err
	}
	if err := egress.CheckURL(u); err != nil {
		return httpTransportResponse{}, httpTransportEgressDenied(u.Hostname(), err)
	}

	var body io.Reader
	if len(request.Body) > 0 {
		body = bytes.NewReader(request.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return httpTransportResponse{}, fmt.Errorf("HTTP_REQUEST_INVALID: %s", httpTransportFailureReason(u.Hostname(), err))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if err := applyHTTPTransportHeaders(req, request.Headers); err != nil {
		return httpTransportResponse{}, err
	}

	resp, err := egress.Client().Do(req)
	if err != nil {
		if errors.Is(err, effects.ErrEgressBlocked) {
			return httpTransportResponse{}, httpTransportEgressDenied(u.Hostname(), err)
		}
		return httpTransportResponse{}, fmt.Errorf("HTTP_REQUEST_FAILED: %s", httpTransportFailureReason(u.Hostname(), err))
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPTransportResponse))
	if err != nil {
		return httpTransportResponse{}, fmt.Errorf("HTTP_REQUEST_READ: %w", err)
	}

	bodyValue, err := decodeCanonicalJSON(respBody)
	if err != nil {
		// Non-JSON bodies are still valid HTTP effect values and are returned
		// as strings, matching the existing generic adapter contract.
		bodyValue = string(respBody)
	}
	return httpTransportResponse{
		Status:  resp.StatusCode,
		Body:    bodyValue,
		Headers: flattenHTTPTransportHeaders(resp.Header),
	}, nil
}

func mergeHTTPTransportQuery(u *url.URL, queryValue any) error {
	query, ok := queryValue.(map[string]any)
	if !ok || len(query) == 0 {
		return nil
	}
	q := u.Query()
	for key, value := range query {
		q.Set(key, httpTransportScalarToString(value))
	}
	encoded := q.Encode()
	if len(encoded) > maxHTTPTransportQuery {
		return errors.New("HTTP_REQUEST_QUERY_TOO_LARGE")
	}
	u.RawQuery = encoded
	return nil
}

func applyHTTPTransportHeaders(req *http.Request, headersValue any) error {
	headers, ok := headersValue.(map[string]any)
	if !ok || len(headers) == 0 {
		return nil
	}
	total := 0
	for name, value := range headers {
		lower := strings.ToLower(strings.TrimSpace(name))
		if _, forbidden := forbiddenHTTPTransportHeaders[lower]; forbidden || strings.HasPrefix(lower, "proxy-") {
			continue
		}
		text := httpTransportScalarToString(value)
		total += len(name) + len(text)
		if total > maxHTTPTransportHeaders {
			return errors.New("HTTP_REQUEST_HEADERS_TOO_LARGE")
		}
		req.Header.Set(name, text)
	}
	return nil
}

func httpTransportScalarToString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case bool:
		return strconv.FormatBool(value)
	case nil:
		return ""
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(data)
	}
}

func flattenHTTPTransportHeaders(headers http.Header) map[string]any {
	out := make(map[string]any, len(headers))
	for name, values := range headers {
		if len(values) > 0 {
			out[name] = values[len(values)-1]
		}
	}
	return out
}

// decodeCanonicalJSON preserves json.Number values so the generic
// completion digest and the direct EffectHandlers output observe the same
// response shape.
func decodeCanonicalJSON(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func httpTransportTimeout(value any) time.Duration {
	var millis float64
	switch value := value.(type) {
	case json.Number:
		millis, _ = value.Float64()
	case float64:
		millis = value
	case int:
		millis = float64(value)
	case int64:
		millis = float64(value)
	}
	if millis <= 0 {
		return defaultHTTPTransportTimeout
	}
	duration := time.Duration(millis) * time.Millisecond
	if duration > maxHTTPTransportTimeout {
		return maxHTTPTransportTimeout
	}
	return duration
}

// httpTransportFailureReason is host-only. In particular, *url.Error's
// string contains the complete request URL and therefore authored query
// values; only its underlying cause is allowed into the error surface.
func httpTransportFailureReason(host string, err error) string {
	if err == nil {
		return "request failed"
	}
	cause := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Err != nil {
			cause = urlErr.Err.Error()
		} else {
			cause = "request failed"
		}
	}
	if host == "" {
		return cause
	}
	return host + ": " + cause
}

func httpTransportEgressDenied(host string, err error) error {
	return fmt.Errorf("EGRESS_BLOCKED: %s", httpTransportFailureReason(host, err))
}
