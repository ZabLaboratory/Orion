// effect_http.go wires `core.effect.invoke@1` invocations whose capability
// is `core.http.request` (StepResult.Invocations, Blue PR #313 — the
// blue.effect.invocation.v1 / blue.effect.completion.v1 async protocol,
// runtime/go effects.go/runtime.go) to a real outbound HTTP call, and
// reports the outcome back to the portable runtime via Runtime.Complete.
//
// Any other capability is left pending — the same as before this file
// existed (Step never even surfaced StepResult.Invocations). db.query@1/
// source.read@1 are explicitly out of scope for this pass (Orion #336).
package bluehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// httpEffectCapability is the only capability this file executes.
const httpEffectCapability = "core.http.request"

// Bounds mirror internal/runtime/exec_effects.go's Bastion §3.7 hardening
// for the same capability (header/query/body caps, response cap, timeout
// ceiling) — same platform posture, applied to the invocation/completion
// payload shape instead of exec pins.
const (
	maxHTTPEffectResponse    = 1 << 20
	maxHTTPEffectHeaders     = 16 << 10
	maxHTTPEffectQuery       = 8 << 10
	maxHTTPEffectBody        = 1 << 20
	defaultHTTPEffectTimeout = 10 * time.Second
	maxHTTPEffectTimeout     = 60 * time.Second
)

// forbiddenHTTPEffectHeaders is the lower-cased set of headers an authored
// `headers` request field may never forward: sensitive credential headers
// and hop-by-hop headers. Orion never forwards Authorization — an effect
// invocation has no caller. Mirrors exec_effects.go's dropForwardHeader.
var forbiddenHTTPEffectHeaders = map[string]struct{}{
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

// SetHTTPEffects wires the deployment's fail-closed egress policy and
// worker pool so Step can execute `core.http.request` invocations for
// real. Unwired (nil egress or nil runner) leaves every such invocation
// pending forever — never a silent capability grant.
func (h *Host) SetHTTPEffects(egress *effects.EgressPolicy, runner *effects.Runner, logger *slog.Logger) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.httpEgress = egress
	h.httpRunner = runner
	if logger != nil {
		h.logger = logger
	}
}

// dispatchInvocations submits every `core.http.request` invocation this
// Step emitted to the worker pool. Each job executes off the caller's
// goroutine and reports back through Runtime.Complete once the real HTTP
// call resolves.
func (h *Host) dispatchInvocations(slot Slot, instance *blueruntime.InstanceHandle, invocations []map[string]any) {
	if len(invocations) == 0 {
		return
	}
	h.mu.Lock()
	egress, runner := h.httpEgress, h.httpRunner
	h.mu.Unlock()
	if egress == nil || runner == nil {
		return
	}
	for _, invocation := range invocations {
		capability, _ := invocation["capability"].(string)
		if capability != httpEffectCapability {
			continue
		}
		inv := invocation
		ok := runner.Submit(effects.Job{
			SceneID: stringField(inv, "instance_id"),
			Timeout: httpEffectTimeout(inv),
			Run: func(ctx context.Context) effects.Result {
				return runHTTPEffect(ctx, egress, inv)
			},
			Deliver: func(res effects.Result) {
				h.completeHTTPEffect(slot, instance, inv, res)
			},
		})
		if !ok {
			// Pool refused (full/stopped): report a completion synchronously
			// so the invocation never stays pending forever — same
			// back-pressure posture as exec_effects.go's EFFECT_QUEUE_FULL.
			h.completeHTTPEffect(slot, instance, inv, effects.Result{Err: "EFFECT_QUEUE_FULL"})
		}
	}
}

// completeHTTPEffect builds the blue.effect.completion.v1 payload for res
// and reports it via Runtime.Complete, guarding against the slot having
// been Released/Take-superseded while the HTTP call was in flight (a stale
// completion for a no-longer-current instance is dropped, not delivered).
func (h *Host) completeHTTPEffect(slot Slot, instance *blueruntime.InstanceHandle, inv map[string]any, res effects.Result) {
	completion, err := buildHTTPCompletion(inv, res)
	if err != nil {
		h.logger.Error("bluehost: failed to build effect completion", "invocation_id", inv["invocation_id"], "err", err)
		return
	}
	data, err := json.Marshal(completion)
	if err != nil {
		h.logger.Error("bluehost: failed to marshal effect completion", "invocation_id", inv["invocation_id"], "err", err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	cur, ok := h.slots[slot]
	if !ok || cur.instance != instance {
		// Slot released or superseded (Take) while the effect was in
		// flight — the instance this completion targets is no longer
		// reachable through the Host API. Dropping it is correct: the
		// runtime's own pendingEffects bookkeeping dies with the instance.
		return
	}
	if _, err := h.runtime.Complete(instance, data); err != nil {
		h.logger.Error("bluehost: Runtime.Complete rejected effect completion", "invocation_id", inv["invocation_id"], "err", err)
	}
}

// httpEffectTimeout reads the request's `timeout_ms` field (the
// core.http.request convention), clamped to maxHTTPEffectTimeout. Absent
// or non-positive falls back to defaultHTTPEffectTimeout.
func httpEffectTimeout(inv map[string]any) time.Duration {
	request, _ := inv["request"].(map[string]any)
	if request != nil {
		if n, ok := request["timeout_ms"].(json.Number); ok {
			if ms, err := n.Int64(); err == nil && ms > 0 {
				d := time.Duration(ms) * time.Millisecond
				if d > maxHTTPEffectTimeout {
					return maxHTTPEffectTimeout
				}
				return d
			}
		}
	}
	return defaultHTTPEffectTimeout
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// runHTTPEffect executes invocation's `request` field as a real outbound
// HTTP call through egress (scheme/host allowlist + post-DNS IP vetting +
// per-hop redirect re-check). Every failure mode — malformed URL, egress
// denial, cap exceeded, transport error — returns an effects.Result with
// Err set; the caller turns that into a "failed" completion, never a
// crash. Mirrors internal/runtime/exec_effects.go's execHTTPRequest
// hardening (header drop, size caps, host-only error messages) adapted to
// the invocation's map[string]any request shape.
func runHTTPEffect(ctx context.Context, egress *effects.EgressPolicy, inv map[string]any) effects.Result {
	request, _ := inv["request"].(map[string]any)
	if request == nil {
		return effects.Result{Err: "HTTP_REQUEST_INVALID: request is not an object"}
	}
	rawURL, _ := request["url"].(string)
	method := strings.ToUpper(stringField(request, "method"))
	if method == "" {
		method = http.MethodGet
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		// Host-only: url.Parse's *url.Error re-echoes the raw URL — never
		// interpolate it raw (it may carry an authored secret query).
		return effects.Result{Err: "HTTP_REQUEST_INVALID_URL: malformed url"}
	}

	var bodyBytes []byte
	if body, ok := request["body"]; ok && body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return effects.Result{Err: "HTTP_REQUEST_ENCODE: " + err.Error()}
		}
	}
	if len(bodyBytes) > maxHTTPEffectBody {
		return effects.Result{Err: "HTTP_REQUEST_BODY_TOO_LARGE"}
	}

	if err := mergeHTTPEffectQuery(u, request["query"]); err != nil {
		return effects.Result{Err: err.Error()}
	}
	if err := egress.CheckURL(u); err != nil {
		return httpEffectEgressDenied(u.Hostname(), err)
	}

	var reader io.Reader
	if len(bodyBytes) > 0 {
		reader = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return effects.Result{Err: "HTTP_REQUEST_INVALID: " + httpEffectFailureReason(u.Hostname(), err)}
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if err := applyHTTPEffectHeaders(req, request["headers"]); err != nil {
		return effects.Result{Err: err.Error()}
	}

	resp, err := egress.Client().Do(req)
	if err != nil {
		if errors.Is(err, effects.ErrEgressBlocked) {
			return httpEffectEgressDenied(u.Hostname(), err)
		}
		return effects.Result{Err: "HTTP_REQUEST_FAILED: " + httpEffectFailureReason(u.Hostname(), err)}
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPEffectResponse))
	if err != nil {
		return effects.Result{Err: "HTTP_REQUEST_READ: " + err.Error()}
	}
	bodyValue, err := decodeCanonicalJSON(respBody)
	if err != nil {
		// Non-JSON body: carry it as a string, same convention as
		// exec_effects.go's asJSON fallback.
		bodyValue = string(respBody)
	}
	headers := map[string]any{}
	for name, values := range resp.Header {
		if len(values) > 0 {
			headers[name] = values[len(values)-1]
		}
	}
	response := map[string]any{
		"status":  json.Number(strconv.Itoa(resp.StatusCode)),
		"body":    bodyValue,
		"headers": headers,
	}
	data, err := json.Marshal(response)
	if err != nil {
		return effects.Result{Err: "HTTP_REQUEST_ENCODE: " + err.Error()}
	}
	return effects.Result{Value: data}
}

// mergeHTTPEffectQuery folds an authored `query` object into u's
// query-string, stringifying scalar values. Enforces maxHTTPEffectQuery.
func mergeHTTPEffectQuery(u *url.URL, queryValue any) error {
	query, ok := queryValue.(map[string]any)
	if !ok || len(query) == 0 {
		return nil
	}
	q := u.Query()
	for k, v := range query {
		q.Set(k, httpEffectScalarToString(v))
	}
	encoded := q.Encode()
	if len(encoded) > maxHTTPEffectQuery {
		return errors.New("HTTP_REQUEST_QUERY_TOO_LARGE")
	}
	u.RawQuery = encoded
	return nil
}

// applyHTTPEffectHeaders sets the authored `headers` object on req,
// dropping sensitive/hop-by-hop names and enforcing maxHTTPEffectHeaders.
func applyHTTPEffectHeaders(req *http.Request, headersValue any) error {
	headers, ok := headersValue.(map[string]any)
	if !ok || len(headers) == 0 {
		return nil
	}
	total := 0
	for name, v := range headers {
		lower := strings.ToLower(strings.TrimSpace(name))
		if _, forbidden := forbiddenHTTPEffectHeaders[lower]; forbidden || strings.HasPrefix(lower, "proxy-") {
			continue
		}
		val := httpEffectScalarToString(v)
		total += len(name) + len(val)
		if total > maxHTTPEffectHeaders {
			return errors.New("HTTP_REQUEST_HEADERS_TOO_LARGE")
		}
		req.Header.Set(name, val)
	}
	return nil
}

func httpEffectScalarToString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case bool:
		if value {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		b, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// httpEffectFailureReason renders a transport failure as a host-only
// string, never the full request URL (a *url.Error's .Error() re-echoes
// it, including any authored query secret).
func httpEffectFailureReason(host string, err error) string {
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

func httpEffectEgressDenied(host string, err error) effects.Result {
	return effects.Result{Err: "EGRESS_BLOCKED: " + httpEffectFailureReason(host, err)}
}

// decodeCanonicalJSON decodes data with json.Number for numbers (matching
// blueruntime's own decoding convention) so the resulting value is
// providers.CanonicalBytes/Digest-compatible.
func decodeCanonicalJSON(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// buildHTTPCompletion builds the blue.effect.completion.v1 envelope for
// inv's outcome res — "succeeded" with the decoded HTTP response, or
// "failed" with a blue.runtime.error.v1 envelope. Uses providers.Digest,
// the same LSML canonicalization algorithm blueruntime.ParseCompletion
// verifies internally (blueruntime does not export its own digest
// functions — see internal/providers/canonical.go's doc comment).
func buildHTTPCompletion(inv map[string]any, res effects.Result) (map[string]any, error) {
	if res.Err != "" {
		return buildFailedHTTPCompletion(inv, res.Err)
	}
	response, err := decodeCanonicalJSON(res.Value)
	if err != nil {
		return buildFailedHTTPCompletion(inv, "HTTP_REQUEST_COMPLETION_DECODE: "+err.Error())
	}
	completion := baseHTTPCompletion(inv, "succeeded")
	responseDigest, err := canonical.Digest(response)
	if err != nil {
		return nil, fmt.Errorf("bluehost: digest http effect response: %w", err)
	}
	completion["response"] = response
	completion["response_digest"] = responseDigest
	return finalizeHTTPCompletion(completion)
}

func buildFailedHTTPCompletion(inv map[string]any, message string) (map[string]any, error) {
	completion := baseHTTPCompletion(inv, "failed")
	code := "PROVIDER_FAILED"
	if strings.HasPrefix(message, "EGRESS_BLOCKED") {
		code = "EGRESS_BLOCKED"
	}
	completion["error"] = map[string]any{
		"schema_version": blueruntime.ErrorSchema,
		"code":           code,
		"stage":          "step",
		"retryable":      false,
		"message":        message,
	}
	return finalizeHTTPCompletion(completion)
}

func baseHTTPCompletion(inv map[string]any, status string) map[string]any {
	return map[string]any{
		"schema_version":     blueruntime.CompletionSchema,
		"invocation_id":      inv["invocation_id"],
		"instance_id":        inv["instance_id"],
		"program_digest":     inv["program_digest"],
		"effect_id":          inv["effect_id"],
		"capability":         inv["capability"],
		"version":            inv["version"],
		"operation":          inv["operation"],
		"mode":               inv["mode"],
		"correlation_id":     inv["correlation_id"],
		"causation_event_id": inv["causation_event_id"],
		"continuation_id":    inv["continuation_id"],
		"attempt":            json.Number("1"),
		"status":             status,
		"completed_at_ms":    json.Number(strconv.FormatInt(time.Now().UnixMilli(), 10)),
	}
}

func finalizeHTTPCompletion(completion map[string]any) (map[string]any, error) {
	digest, err := canonical.Digest(completion)
	if err != nil {
		return nil, fmt.Errorf("bluehost: digest http effect completion: %w", err)
	}
	completion["completion_digest"] = digest
	return completion, nil
}
