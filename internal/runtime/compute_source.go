package runtime

// core.source.read@1 — introspection compute (ADR 012 Option B).
//
// source.read is NOT an external fetch: it reads a DECLARED source's
// descriptor and emits it. The compiler resolves the authored `source_id`
// against the scene's ExternalAdapter set at COMPILE and folds the
// resolved projection into the node config under the reserved key
// `__resolved_source` (compiler-injected; `__`-prefixed, never an authored
// config key). The runtime compute is therefore a TOTAL pure function over
// `(inputs={}, config)` — no I/O, no http client, no Scene, no Bindings
// lookup. An undeclared source can no longer be a runtime outcome: it is a
// push-time structural reject (SOURCE_NOT_DECLARED, compiler), so by the
// time this runs `__resolved_source` is always present and well-formed.
//
// Multi-output shape (ADR 012 §2.1, Option A — Conduit-recommended,
// Vigil-reviewed): the seed declares four output pins
// (config/descriptor/kind/name) but the compute model is single-value (the
// recompute loop writes ONE value to the node's leaf — scene.go computeAt).
// This compute returns the whole `__resolved_source` object
// `{name, kind, descriptor, config}` as that one value; a blueprint that
// wants an individual pin projects it downstream with core.data.get-field@1
// (which reads `config.path`). This keeps ComputeFn and the shared
// recompute path UNCHANGED — zero new runtime machinery — and matches how
// multi-field data is already consumed downstream (the db.* plan object,
// http.request body object). The runtime hardcodes NO output-pin names, so
// it is trivially a subset of the seed signature (exec-port-parity).

import (
	"encoding/json"
)

// resolvedSourceConfigKey is the reserved compiler-injected config key the
// compiler folds the pre-resolved source descriptor into (ADR 012 §1.2).
// Double-underscore marks it compiler-injected, never an authored key —
// the same convention as the `__`-prefixed internal leaves.
const resolvedSourceConfigKey = "__resolved_source"

// registerSourceTranche adds core.source.read@1 to the registry. Called
// from NewComputeRegistry alongside the pure / db tranches — same seam,
// same total-pure contract.
func (r *ComputeRegistry) registerSourceTranche() {
	r.fns["core.source.read@1"] = sourceReadFn
}

// sourceReadFn — core.source.read@1. Returns the pre-resolved source
// object the compiler folded into config[`__resolved_source`]. Total: a
// missing/malformed key (which the compiler's push-time SOURCE_NOT_DECLARED
// gate makes unreachable in practice) yields JSON null rather than an error
// — matching the core.db.* totality rule (compute_db.go). Ignores inputs
// (the seed declares none).
func sourceReadFn(_, config map[string]json.RawMessage) (json.RawMessage, error) {
	raw, ok := config[resolvedSourceConfigKey]
	if !ok || len(raw) == 0 {
		return json.RawMessage(`null`), nil
	}
	return raw, nil
}
