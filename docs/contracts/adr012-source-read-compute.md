# ADR 012 (Option B) — `core.source.read@1` reclassified to compute — bundle contract

> Conduit-finalized wire/registry contract for the `source.read` reclassification.
> BLOCKING step of the chain: this fixes the pre-resolved descriptor shape and the
> producer↔consumer wire BEFORE Forge touches the runtime. Proven against BOTH
> sides of the real code (compiler producer + `ComputeRegistry` consumer + the
> seed signature), 2026-06-13. NOT self-merged: the diff is Vigil-reviewed in its
> repo; Eleven decides the merge. This doc implements nothing.

---

## 0. Decision recap (porteur-confirmed)

`core.source.read@1` is **introspection** — read a DECLARED internal source /
datasource and emit its descriptor — **NOT an external fetch**. ADR 012 Option B
reclassifies it from an exec-op (`KindExecOp`, world-touching async effect) to a
**pure compute** (`KindCompute`), resolved at COMPILE into a complete descriptor
carried by the bundle, so execution is a pure function with **no I/O, no client,
no `Scene`**.

### Why the reclassification is the *correct* shape, not just a refactor (proven)

The seed already declares it pure. Two independent in-repo facts prove the current
exec-op is the anomaly, not the target:

1. **The seed declares NO exec pins.** `internal/conformance/signature_parity_test.go:96-98`
   states verbatim: *"source.read still declares NO exec pins in the seed (pure
   dataflow — emits a descriptor); its runtime then/error firing is a dead exec
   path."* An exec-op with no exec pins is unreachable as an effect by construction.
2. **The seed output set is a descriptor, not a fetched body.**
   `internal/conformance/signatures.json` declares
   `core.source.read@1` → `inputs: []`, `config: ["source_id"]`,
   `outputs: ["config", "descriptor", "kind", "name"]`.
   The current `execSourceRead` (`exec_effects.go:706`) instead does an HTTP GET on
   a binding URL and binds a single `<node>.value` pin — it serves NEITHER the
   declared inputs (none) NOR the declared outputs (4 introspection pins). The exec
   implementation has been **out of parity with its own seed signature**; Option B
   closes that gap by implementing what the seed already promises.

> Consequence: this is not a behaviour change visible to any live scene. The whole
> exec path is **R9-dormant** (no production scene installs an ExecProgram until
> #87 / ADR 006); `source.read` has never run a real fetch on air. There is no
> live consumer of the `value` pin to break. See §5.

---

## 1. The pre-resolved descriptor — bundle/graph shape (the producer side)

### 1.1 What `source_id` resolves against

`source.read`'s only config key is `source_id`. It names a **declared source** —
today an entry in `Graph.Bindings` / `RenderBundle.ExternalAdapters`
(`internal/compiler/types.go:174`, `ExternalAdapter`):

```go
type ExternalAdapter struct {
    Key         string   `json:"key"`           // ← source_id matches this
    Label       string   `json:"label"`
    Kind        string   `json:"kind"`          // http-poll | pg-listen | platform-stream | tick | …
    TargetPaths []string `json:"target_paths"`
    FrequencyHz *float64 `json:"frequency_hz,omitempty"`
    URL         string   `json:"url,omitempty"`
    Channel     string   `json:"channel,omitempty"`
}
```

The current exec-op already resolves by `b.Key == source_id` (`exec_effects.go:715-722`).
Option B keeps that same correspondence but moves the resolution to **compile time**.

### 1.2 The pre-resolved descriptor lands in the node `Config` (no new bundle field)

The compiler emits compute nodes as `GraphNode` with `Kind: "computed"`,
`Compute: "core.source.read@1"`, and a `Config map[string]json.RawMessage` carried
verbatim into the artefact (`internal/compiler/graph.go:50-81`). The `ComputeFn`
contract is exactly `(inputs, config map[string]json.RawMessage) → (json.RawMessage, error)`
(`internal/runtime/compute.go:18`) — config-bearing pure computes already read
their resolved config this way (e.g. `core.db.from@1` reads `config["table"]`).

**Decision: the resolved descriptor is folded into the node's `Config` at compile —
no new field on `Graph` or `RenderBundle`.** This mirrors how `core.db.*` and
`core.data.get-field` already carry everything they need in `Config`, keeps the
compute a pure function of `(inputs={}, config)`, and adds zero wire surface.

The compiler, at compile, looks up the `ExternalAdapter` whose `Key == source_id`
and folds its introspection projection into the node config under a reserved key
`__resolved_source` (double-underscore = compiler-injected, never an authored
config key — same convention as the `__`-prefixed internal leaves):

```jsonc
// GraphNode.Config for a source.read node, AFTER compile:
{
  "source_id": "leaguepedia_feed",          // authored, preserved verbatim
  "__resolved_source": {                      // compiler-injected, pre-resolved
    "name":       "leaguepedia_feed",         // = ExternalAdapter.Key (echo of source_id)
    "kind":       "http-poll",                // = ExternalAdapter.Kind
    "descriptor": {                            // the introspection descriptor (§1.3)
      "label":        "Leaguepedia feed",
      "target_paths": ["__inputs.feed.leagues"],
      "frequency_hz": 5.0,
      "channel":      null
    }
  }
}
```

### 1.3 The four output pins ↔ resolved descriptor (the parity table)

The seed declares outputs `config / descriptor / kind / name`. The compute binds
each from the pre-resolved `__resolved_source` — a **total** function (introspection
of a present descriptor never errors; an unknown `source_id` is handled per §1.4):

| Seed output pin | Bound value (from `__resolved_source`) | Source field |
|---|---|---|
| `name`       | `__resolved_source.name`               | `ExternalAdapter.Key` |
| `kind`       | `__resolved_source.kind`               | `ExternalAdapter.Kind` |
| `descriptor` | `__resolved_source.descriptor`         | projection of label/target_paths/frequency_hz/channel |
| `config`     | `__resolved_source.descriptor` (alias) OR the authored adapter config object | see note |

> **Flag for Forge/Vigil (output-pin semantics, minor):** the seed declares BOTH
> `config` and `descriptor` as outputs. Their distinction is not pinned by the
> current code (the exec-op binds neither — it binds `value`). Recommended: `descriptor`
> = the structural projection above; `config` = the same object (alias) until a
> blueprint actually consumes them differently. If a real consumer needs them
> distinct, that is a **seed-signature question for Blue**, not a runtime call —
> flag to Eleven. The compute MUST bind all four declared pins (subset rule of
> `TestExecPortParity_RuntimeStringsExistInSeed`).

### 1.4 Unknown `source_id` — total, no error port

A compute has no `error` exec pin (it is pure dataflow). An unresolved `source_id`
(no matching `ExternalAdapter.Key`) MUST be a **compile-time structural rejection**
— mirroring `DATASOURCE_NOT_DECLARED` for `db.query` (`exec_effects.go:154`,
`ValidateExecDataSources`). Recommended code: `SOURCE_NOT_DECLARED` (reuses the
existing runtime error string the exec-op used, now raised at compile, not runtime).
The compute itself, given a present `__resolved_source`, is **total** (never errors)
— matching the `core.db.*` totality rule (`compute_db.go:14-22`).

> Rationale: resolution moves to compile, so an undeclared source can no longer be
> a runtime `error`-port outcome — there is no error port. It becomes a push-time
> reject, which is strictly safer (fails at `POST /push`, never on air).

---

## 2. Producer ↔ consumer coherence (the wire, proven both sides)

| Concern | Producer (compiler, `internal/compiler`) | Consumer (registry, `internal/runtime`) |
|---|---|---|
| Node kind | emits `GraphNode{Kind:"computed", Compute:"core.source.read@1"}` | classified `KindCompute`; resolved via `cmpReg.Get(node.Compute)` |
| Resolution | folds matching `ExternalAdapter` into `Config["__resolved_source"]` at compile | reads `config["__resolved_source"]` — pure, no lookup, no I/O |
| Inputs | none (seed `inputs: []`) | `inputs` map is empty; compute ignores it |
| Outputs | seed `["config","descriptor","kind","name"]` | binds all four from `__resolved_source` |
| Undeclared source | reject at compile `SOURCE_NOT_DECLARED` | never reached (compile already gated) |
| Purity | `IsPure: true` mirrored from Blue manifest onto the `GraphNode` | registered in `NewComputeRegistry`, executed by the `Kind != "input"` recompute loop |

The `ComputeFn` signature is the contract boundary and it is satisfied by
construction: `func(inputs, config map[string]json.RawMessage) (json.RawMessage, error)`.
One subtlety — **a compute returns ONE value** (its output leaf `Path`), but the
seed declares FOUR output pins. See §2.1.

### 2.1 Multi-output compute — the one real wire question for Forge

Every existing compute is single-output (`ComputeFn` returns one `json.RawMessage`
written to the node's `Path`). `source.read` declares four output pins. Two
coherent shapes — **Forge picks with Vigil, both are in-contract**:

- **(A) single object value, downstream `get-field`.** The compute returns one
  object `{"name":…, "kind":…, "descriptor":…, "config":…}`; the four pins are
  projected by the compiler wiring each consumer edge through the existing
  `core.data.get-field@1` (which already reads `config.path`). Zero new runtime
  machinery — reuses the proven single-value compute path. **Recommended.**
- **(B) genuine multi-out compute.** Extend the recompute loop to bind
  `<node>.<pin>` for a declared output set, like the exec data-out pins
  (`GraphInput.FromPort`, `graph.go:87-101`). Larger blast radius (touches the
  pure recompute path that every compute shares).

Recommend **(A)**: it keeps `ComputeFn` unchanged and matches how multi-field data
is already consumed downstream. Flag to Eleven if a blueprint genuinely wires the
four pins independently (then B, scoped as its own issue).

---

## 3. S2 — sites of retrait for Forge (exec-op → compute)

Forge MUST remove `source.read` from EVERY exec-layer registration and add it to
the compute registry. Enumerated and verified against the tree (paths are
`internal/runtime`, per Vigil S1 — NOT `internal/engine`):

| # | Site | File:line | Action |
|---|---|---|---|
| R1 | `ExecOps` canonical list | `internal/runtime/exec.go:448` | remove `OpSourceRead` from the slice |
| R2 | `worldEffectRegistrations` table | `internal/runtime/exec_effects.go:99-106` | remove the `{OpSourceRead, execSourceRead}` entry (single source of truth — removing it auto-drops it from `worldEffectOps`, `EnumerateWorldEffects`, and `registeredWorldEffectOps`, exec_validation.go) |
| R3 | `execSourceRead` impl + `OpSourceRead` const + `SourceClient` field | `internal/runtime/exec_effects.go:41, 81-85, 695-755` | delete `execSourceRead`; delete the `OpSourceRead` const; **drop the `SourceClient *http.Client` field from `SceneEffects`** (its sole consumer was `execSourceRead`) — verify no other reader before deleting |
| R4 | Validation-mode synthetic result | `internal/runtime/exec_validation.go:85-87` | remove the `case OpSourceRead` from `validationSyntheticResult` (no longer a world op) |
| R5 | Validation-mode finish binding | `internal/runtime/exec_validation.go:187-189` | remove the `case OpSourceRead` from `validationFinish` |
| R6 | Conformance classification | `internal/conformance/conformance.go:170` | change `core.source.read@1` from `{Kind: KindExecOp, Op: "source.read", …}` to `{Kind: KindCompute, Test: "<new compute test>"}` |
| R7 | Manifest mirror | `internal/conformance/manifest.json:654` (+ `internal/compiler/testdata/blue_compute_manifest.json:45`) | `is_pure` becomes `true` to match the reclassification — **this MUST be reconciled with Blue's seeded manifest** (the authoritative source). Flag: if Blue still seeds `is_pure: false`, the manifest gate and Blue diverge → cross-repo Blue change required (see §4) |
| R8 | Test harness — exec-op tests | `internal/runtime/exec_effects_test.go:391-413` (`TestEffects_SourceReadDeclaredBinding`), `exec_effects_wiring_test.go:94,118-144`, `validation_harness_probe_test.go:147-197`, `conformance_exec_test.go:103,170-171` | rewrite/remove: source.read is no longer an effect. `TestEffects_SourceReadDeclaredBinding` (HTTP-fetch test) is deleted; replace with a compute test driving `(inputs={}, config)` through `NewComputeRegistry().Get("core.source.read@1")` |
| R9 | Canary / e2e mentions | `tests/e2e/canary_firstflight_test.go:28,117,155`, `internal/runtime/canary_firstflight_harness_test.go:29` | update the "no http.request/db.query/source.read on air" assertions — source.read is no longer in that world-op set (it can now run on air as a pure compute, which is correct) |

Registration (S3): add `source.read` in `NewComputeRegistry`
(`internal/runtime/compute.go:45`) — the single seam the registry grows by. Place
it in its own tranche or alongside the pure tranche; it reads
`config["__resolved_source"]` and is a total pure function. The conformance matrix
job (`conformance-matrix` CI) then asserts the registered compute has a passing
test — that is the executable proof, per criterion 1.

---

## 4. Cross-repo flag (Blue manifest parity — for Eleven)

The Blue stdlib seeder (`Blue/src/blue/services/stdlib_seeder.py`) is the
**authoritative** manifest source (`compute.go:36`). Today it seeds
`core.source.read@1` with `is_pure: false` (mirrored in Orion's `manifest.json:659`).
Option B makes it `is_pure: true`. **The Orion `manifest.json` is a mirror, not the
source** — flipping it in Orion alone makes the mirror lie. Either:

- the Blue seed must flip to `is_pure: true` (a small Blue change, separate PR,
  separate repo — Forge-on-Blue or Scribe-adjacent), OR
- ADR 012 explicitly scopes the manifest reconciliation and Eleven sequences the
  Blue change BEFORE the Orion conformance flip (producer-before-consumer ordering).

This is the one genuinely inter-repo coupling. It is **not** rétro-incompatible at
runtime (the path is R9-dormant), but the manifest mirror and the seed MUST agree
or the manifest-parity check drifts. **Recommended order:** Blue seed flip →
Orion conformance/manifest flip → Orion runtime reclassification. Eleven decides.

No ZabGate / routing / port / header surface is touched — `source.read` becoming a
compute removes a (dormant) egress path entirely; it does not add one. No Bastion
veto surface is created (it removes one world-effect op).

---

## 5. Non-regression / smoke — what proves this, and its honest limit

- **Unit/conformance (executable, runnable now):** the new compute test driving
  `NewComputeRegistry().Get("core.source.read@1")` over `(inputs={}, config)` +
  the `conformance-matrix` CI job (asserts every served node has a registered
  executor + passing test) + `TestExecPortParity_*` (the four output pins are a
  subset of the seed signature). This is the layer Forge must turn green.
- **Live (deferred, declared blind spot):** there is **no live smoke** to run for
  this change, and that is correct, not a gap. The entire exec/source.read path is
  **R9-dormant** — no production scene installs an ExecProgram until #87 (ADR 006).
  `source.read` has never executed a real fetch on air; no live scene reads its
  output. A live Twitch smoke (live-testing.md objective measure) would exercise
  nothing this change touches. **Declared blind spot:** the first time a real scene
  authors a `source.read` node and runs it on air, the introspection output must be
  re-verified end-to-end. That belongs with the #87 dormancy lift, not here.
- **Gateway:** unaffected. `GET /orion/api/v1/render-bundle` byte-stability
  (criterion 3) holds — the new `__resolved_source` config only appears on scenes
  that author a `source.read` node (none today), so existing bundle hashes are
  byte-identical.

---

## 6. Verdict

**Aligné, sous deux conditions sequencées par Eleven:**

1. **Blue manifest parity (§4):** `is_pure: true` must land in the Blue seed (the
   authoritative source) before/with Orion's manifest mirror flip — else the mirror
   lies. Producer-before-consumer.
2. **Multi-output shape (§2.1):** Forge picks (A) get-field projection (recommended,
   zero new machinery) or (B) multi-out compute (own issue) with Vigil.

Everything else is internal to Orion, R9-dormant, and removes a world-effect op
rather than adding surface. `KindCompute` / `NewComputeRegistry` confirmed as the
target seam (S3). No ADR amendment, no Bastion veto surface, no gateway change.
