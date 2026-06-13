# ADR 012 — `core.source.read@1` reclassified to pure compute (Option B)

- **Status**: accepted
- **Date**: 2026-06-13
- **Decided**: 2026-06-13
- **Deciders**: @ClodoCapeo
- **Author**: Conduit (wire contract) · Forge (runtime reclassification, PR #166)
- **Supersedes**: the `execSourceRead` exec-op from ADR 003 phase-3 (PR #97) —
  that implementation was out of parity with its own seed signature (it fetched
  a URL and bound `value`; the seed declared four introspection pins and no exec
  pins).
- **Superseded by**: —

> **Numbering.** 011 is animation-play keyframe lowering. First free after 011: **012**.

---

## 1. Context

`core.source.read@1` is an introspection primitive: given a `source_id`, it
reads the **declared** source/datasource and emits its descriptor. It is NOT an
external fetch.

Two independent source facts proved the exec-op classification was wrong
(verified against the actual code, 2026-06-13):

1. **The seed declares no exec pins.** `signature_parity_test.go:96-98` states
   verbatim: *"source.read still declares NO exec pins in the seed (pure
   dataflow — emits a descriptor); its runtime then/error firing is a dead exec
   path."* An exec-op with no exec pins is unreachable as an effect by
   construction.
2. **The seed output set is a descriptor, not a fetched body.** `signatures.json`
   declares outputs `["config", "descriptor", "kind", "name"]`. The
   `execSourceRead` implementation (`exec_effects.go:706`) instead did an HTTP
   GET on a binding URL and bound a single `<node>.value` pin — serving neither
   the declared inputs (none) nor the declared outputs (four introspection pins).

The exec implementation was out of parity with its own seed signature. Option B
closes that gap by implementing what the seed already promised.

> **Dormancy**: the exec path was **R9-dormant** (no production scene installs
> an ExecProgram until issue #87 / ADR 006 lift). `source.read` has never run a
> real fetch on air. There is no live consumer of the `value` pin to break.

Full wire contract: `docs/contracts/adr012-source-read-compute.md` (Conduit,
proven both sides, 2026-06-13).

---

## 2. Decision — reclassify to `KindCompute` with compile-time resolution

`core.source.read@1` is reclassified from `KindExecOp` to `KindCompute`.
Resolution moves from runtime to **compile time**: the compiler folds the
matched `ExternalAdapter` into the node's `Config` under the reserved key
`__resolved_source`. The compute is a pure function with no I/O, no client,
no `Scene`.

### 2.1 Bundle shape — `__resolved_source` in node Config

At `POST /push` compile, the compiler looks up the `ExternalAdapter` whose
`Key == source_id` and folds its introspection projection into `Config`:

```jsonc
// GraphNode.Config for source.read after compile
{
  "source_id": "leaguepedia_feed",         // authored, preserved verbatim
  "__resolved_source": {                    // compiler-injected
    "name":       "leaguepedia_feed",
    "kind":       "http-poll",
    "descriptor": {
      "label":        "Leaguepedia feed",
      "target_paths": ["__inputs.feed.leagues"],
      "frequency_hz": 5.0,
      "channel":      null
    }
  }
}
```

No new field on `Graph` or `RenderBundle` — the descriptor rides the existing
`Config map[string]json.RawMessage` mechanism, matching `core.db.*` and
`core.data.get-field`.

### 2.2 Output pins — four descriptor projections

| Seed output pin | Bound value |
|---|---|
| `name`       | `__resolved_source.name` (`ExternalAdapter.Key`) |
| `kind`       | `__resolved_source.kind` |
| `descriptor` | structural projection of label / target_paths / frequency_hz / channel |
| `config`     | alias of `descriptor` until a blueprint consumes them differently |

`config` and `descriptor` are aliased for now; if a blueprint consumer needs
them distinct, that is a Blue seed-signature question — flag to Eleven.

### 2.3 Unknown `source_id` — compile-time rejection

A `source_id` that does not match any declared `ExternalAdapter.Key` is
rejected at `POST /push` with `SOURCE_NOT_DECLARED`. Mirrors
`DATASOURCE_NOT_DECLARED` for `db.query`. The compute is **total** given a
present `__resolved_source` (never errors at runtime).

### 2.4 Multi-output shape — Option A (get-field projection, recommended)

The compute returns a single object `{"name":…, "kind":…, "descriptor":…,
"config":…}`; the four output pins are projected by the compiler wiring each
consumer edge through the existing `core.data.get-field@1`. Zero new runtime
machinery. Option B (genuine multi-out compute) is available as a follow-up if
a blueprint independently wires all four pins (Forge + Vigil gate that decision).

---

## 3. Sites of change (exec-op → compute)

Enumerated in `docs/contracts/adr012-source-read-compute.md §3`. Summary:

- Remove `OpSourceRead` from `ExecOps` list and `worldEffectRegistrations`.
- Delete `execSourceRead`, `OpSourceRead` const, and `SourceClient *http.Client`
  field from `SceneEffects` (sole consumer was `execSourceRead`).
- Remove validation-mode synthetic result and finish binding for `OpSourceRead`.
- Change conformance classification from `KindExecOp` to `KindCompute`.
- Flip `is_pure: true` in `manifest.json` (mirror — see §4 on Blue parity).
- Add to `NewComputeRegistry` in `internal/runtime/compute.go`.
- Rewrite/remove exec-op tests; add compute test over `(inputs={}, config)`.
- Update canary / e2e assertions: `source.read` is no longer in the world-op
  set (it can run on air as a pure compute — correct).

---

## 4. Cross-repo dependency — Blue manifest parity

The Blue stdlib seeder (`Blue/src/blue/services/stdlib_seeder.py`) is the
**authoritative** source. Today it seeds `is_pure: false` for
`core.source.read@1`; Orion's `manifest.json` mirrors that. Option B makes it
`is_pure: true`. The Orion mirror must not flip ahead of the authoritative
source.

**Required sequence (Eleven gates)**:

1. Blue PR: seed `core.source.read@1` with `is_pure: true`.
2. Orion PR: flip `manifest.json` mirror + runtime reclassification.

The path is R9-dormant; the sequence is not urgent but must be respected to
keep the mirror honest.

---

## 5. Security

`source.read` becoming a compute **removes** a (dormant) world-effect op — it
does not add surface. No new egress path is created; the egress path
(`execSourceRead`'s HTTP GET) is deleted. No Bastion veto surface. The
`__resolved_source` compiler key is double-underscored (compiler-injected
convention) and never writable by an authored blueprint config.

---

## 6. Resolution criteria

1. `core.source.read@1` is registered in `NewComputeRegistry`; the conformance
   matrix CI job (`conformance-matrix`) asserts a passing compute test.
2. The four declared output pins (`config`, `descriptor`, `kind`, `name`) are
   bound by the compute; `TestExecPortParity_RuntimeStringsExistInSeed` passes.
3. A push of a blueprint with `source_id` matching a declared source compiles
   successfully; the `GraphNode.Config` carries `__resolved_source` with the
   correct adapter fields.
4. A push with an undeclared `source_id` is rejected at `POST /push` with
   `SOURCE_NOT_DECLARED`.
5. `execSourceRead` and `SourceClient` are absent from the compiled binary
   (exec-op tombstoned).
6. The canary / e2e `source.read` assertions are updated to reflect the
   compute classification (no longer a world-op).
7. Blue seed `is_pure: true` has landed in Blue before Orion's manifest mirror
   is flipped (sequencing condition — Eleven gates).
