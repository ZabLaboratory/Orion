// effect_http.go wires `core.effect.invoke@1` invocations whose capability
// is `core.http.request` (StepResult.Invocations, Blue PR #313 — the
// blue.effect.invocation.v1 / blue.effect.completion.v1 async protocol,
// runtime/go effects.go/runtime.go) to a real outbound HTTP call, and
// reports the outcome back to the portable runtime via Runtime.Complete.
//
// Any other capability is left pending — this adapter only owns
// core.http.request. A core.http.request invocation that cannot be dispatched
// because its bundle is incomplete is completed through the same protocol
// with EFFECT_PROVIDER_UNAVAILABLE; db.query@1/source.read@1 are explicitly
// out of scope for this pass (Orion #336).
//
// MODE GATE (ORION-PREVIEW-EFFECT-GATE): dispatchInvocations only dials for
// Execute-mode slots (the antenna). See its own doc for why.
package bluehost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

// httpEffectCapability is the only capability this file executes.
const httpEffectCapability = "core.http.request"

const httpEffectDependenciesUnavailable = "EFFECT_PROVIDER_UNAVAILABLE: HTTP effect dependencies are not configured"

// SetHTTPEffects wires the deployment's fail-closed egress policy and
// worker pool so Step can execute `core.http.request` invocations for
// real. Unwired (nil egress or nil runner) completes such invocations with a
// provider failure through Runtime.Complete — never a silent capability grant
// or an invocation left pending indefinitely.
func (h *Host) SetHTTPEffects(deps EffectDeps, logger *slog.Logger) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.httpEgress = deps.Egress
	h.httpRunner = deps.Runner
	if logger != nil {
		h.logger = logger
	}
}

// dispatchInvocations submits every `core.http.request` invocation this
// Step emitted to the worker pool. Each job executes off the caller's
// goroutine and reports back through Runtime.Complete once the real HTTP
// call resolves.
//
// Preview NEVER dials the network — same posture NewEffectHandlers already
// applies to the 4 opcodes of full right (effects.go: "blueruntime.Preview
// NEVER dials the network or the DB") and dispatchOverlayAppSet applies to
// the wire effector (effect_overlay.go). `core.effect.invoke@1` is the third
// path an instance can reach the network through — StepResult.Invocations,
// the async admission protocol — and had no such gate: a preview instance
// invoking core.http.request would have dispatched a REAL outbound call.
// Gated first, before even reading h.httpEgress/h.httpRunner, mirroring
// dispatchOverlayAppSet's placement exactly (ORION-PREVIEW-EFFECT-GATE).
func (h *Host) dispatchInvocations(slot Slot, instance *blueruntime.InstanceHandle, invocations []map[string]any) {
	if modeFor(slot) != blueruntime.Execute {
		return
	}
	if len(invocations) == 0 {
		return
	}
	h.mu.Lock()
	egress, runner := h.httpEgress, h.httpRunner
	h.mu.Unlock()
	if egress == nil || runner == nil {
		for _, invocation := range invocations {
			if stringField(invocation, "capability") != httpEffectCapability {
				continue
			}
			h.completeHTTPEffect(slot, instance, invocation, effects.Result{Err: httpEffectDependenciesUnavailable})
		}
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

	h.runtimeMu.Lock()
	h.mu.Lock()
	cur, ok := h.slots[slot]
	if !ok || cur.instance != instance {
		h.mu.Unlock()
		h.runtimeMu.Unlock()
		// Slot released or superseded (Take) while the effect was in
		// flight — the instance this completion targets is no longer
		// reachable through the Host API. Dropping it is correct: the
		// runtime's own pendingEffects bookkeeping dies with the instance.
		return
	}
	h.mu.Unlock()
	if _, err := h.runtime.Complete(instance, data); err != nil {
		h.logger.Error("bluehost: Runtime.Complete rejected effect completion", "invocation_id", inv["invocation_id"], "err", err)
	}
	h.runtimeMu.Unlock()
}

// httpEffectTimeout reads the request's `timeout_ms` field (the
// core.http.request convention), clamped by the shared transport timeout
// bound. Absent or non-positive values use the shared default.
func httpEffectTimeout(inv map[string]any) time.Duration {
	request, _ := inv["request"].(map[string]any)
	if request == nil {
		return httpTransportTimeout(nil)
	}
	return httpTransportTimeout(request["timeout_ms"])
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

	var bodyBytes []byte
	if body, ok := request["body"]; ok && body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return effects.Result{Err: "HTTP_REQUEST_ENCODE: " + err.Error()}
		}
	}
	response, err := executeHTTP(ctx, egress, httpTransportRequest{
		URL:     stringField(request, "url"),
		Method:  stringField(request, "method"),
		Query:   request["query"],
		Headers: request["headers"],
		Body:    bodyBytes,
	})
	if err != nil {
		return effects.Result{Err: err.Error()}
	}
	data, err := json.Marshal(response.values())
	if err != nil {
		return effects.Result{Err: "HTTP_REQUEST_ENCODE: " + err.Error()}
	}
	return effects.Result{Value: data}
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
