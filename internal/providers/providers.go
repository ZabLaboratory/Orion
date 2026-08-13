// Package providers builds Orion's Zab-side blue.capability-provider.v1
// descriptors (ADR-BLUE-012 §4.3/§6.6) — the "menu" bluehost.Host.Prepare/
// Take pass to blueruntime.StartOptions.Providers so a program declaring
// `requires` can be admitted by checkProviders. Blue defines the abstract
// capability shape; this package is the Zab adaptation for the three
// capabilities Orion currently serves: HTTP egress, stream-level output
// emission, and overlay-app configuration (no store).
package providers

import (
	"encoding/json"
	"strconv"
)

// num builds the json.Number the canonical LSML writer requires — a plain
// int/float64 in a provider descriptor fails canonicalBytes ("unsupported
// JSON value") because writeCanonical only understands json.Number.
func num(n int64) json.Number {
	return json.Number(strconv.FormatInt(n, 10))
}

// httpRequestProvider is the `core.http.request` capability (ADR 010's
// canonical HTTP executor, re-exposed as a provider for the stateless
// bluehost path). Host-side host allowlisting is enforced by Policy, not
// here — this descriptor only advertises the capability's shape.
func httpRequestProvider() map[string]any {
	return map[string]any{
		"schema_version": "blue.capability-provider.v1",
		"capability":     "core.http.request",
		"version":        "1",
		"health":         "healthy",
		"operations": []any{
			map[string]any{
				"name":          "request",
				"request_type":  "core.json",
				"response_type": "core.json",
				"preview":       "noop",
				"execute":       "allowed",
				"idempotency":   "unsupported",
				"cancellation":  "unsupported",
				"deadline":      "unsupported",
				"limits": map[string]any{
					"max_in_flight":     num(8),
					"max_payload_bytes": num(1 << 20),
					"max_deadline_ms":   num(0),
				},
				"backpressure": "reject",
				"error_codes":  []any{"EGRESS_BLOCKED", "PROVIDER_FAILED"},
			},
		},
	}
}

// showEmitProvider is the `core.show.emit` capability — the stateless-path
// successor of ADR 009's `core.show.emit@1` stream-level output primitive.
func showEmitProvider() map[string]any {
	return map[string]any{
		"schema_version": "blue.capability-provider.v1",
		"capability":     "core.show.emit",
		"version":        "1",
		"health":         "healthy",
		"operations": []any{
			map[string]any{
				"name":          "emit",
				"request_type":  "core.json",
				"response_type": "core.json",
				"preview":       "noop",
				"execute":       "allowed",
				"idempotency":   "unsupported",
				"cancellation":  "unsupported",
				"deadline":      "unsupported",
				"limits": map[string]any{
					"max_in_flight":     num(32),
					"max_payload_bytes": num(1 << 18),
					"max_deadline_ms":   num(0),
				},
				"backpressure": "queue",
				"error_codes":  []any{"PROVIDER_FAILED"},
			},
		},
	}
}

// overlayAppProvider is the `core.overlay-app` capability — invoking an
// overlay-hosted app surface. Explicitly stateless: no store backs an
// overlay-app instance, this provider is a pure invocation contract.
// operation "set" mirrors the existing Blue node core.overlay-app.set@1
// (verb-mirrors-action pattern) — NOT "invoke", so a future `requires`
// emitted for that node matches this descriptor exactly.
func overlayAppProvider() map[string]any {
	return map[string]any{
		"schema_version": "blue.capability-provider.v1",
		"capability":     "core.overlay-app",
		"version":        "1",
		"health":         "healthy",
		"operations": []any{
			map[string]any{
				"name":          "set",
				"request_type":  "core.json",
				"response_type": "core.json",
				"preview":       "emulated",
				"execute":       "allowed",
				"idempotency":   "unsupported",
				"cancellation":  "unsupported",
				"deadline":      "unsupported",
				"limits": map[string]any{
					"max_in_flight":     num(16),
					"max_payload_bytes": num(1 << 18),
					"max_deadline_ms":   num(0),
				},
				"backpressure": "reject",
				"error_codes":  []any{"PROVIDER_FAILED"},
			},
		},
	}
}

// Registry returns Orion's full Zab capability-provider catalogue, ready to
// pass as blueruntime.StartOptions.Providers via bluehost.Host.Prepare/Take.
// A fresh slice per call — callers own their copy, mirroring the portable
// runtime's own "providers are copied at admission" contract.
func Registry() []map[string]any {
	return []map[string]any{
		httpRequestProvider(),
		showEmitProvider(),
		overlayAppProvider(),
	}
}
