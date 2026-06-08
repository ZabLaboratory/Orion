# ADR 001 — Multi-blueprint push envelope (N distinct Blue blueprints per scene)

- **Status**: accepted
- **Date**: 2026-06-08
- **Decided**: 2026-06-08
- **Deciders**: @ClodoCapeo (maintainer), Vigil (review), Conduit (contract), Bastion (security clearance)
- **Author**: Atlas (architect agent)
- **Supersedes**: —
- **Superseded by**: —

---

> **Why this ADR lives in `Orion/docs/adr/` and is numbered 001.** The breaking
> change is owned by Orion: the contract is `internal/compiler/types.go::PushEnvelope`
> and the compiler that consumes it. Orion's `CLAUDE.md` references ADR 004/005/007
> as `../docs/adr/…`, but **no such files exist on disk** (they are design references,
> not committed artefacts) — Orion has no physical `docs/adr/` today. Every other Zab
> repo (Prism, Pulsar, ZabCanvas, Zablab) keeps ADRs at `<repo>/docs/adr/NNN-*.md`.
> This is the **first committed ADR in the Orion repo**, hence `001`, following the
> per-repo convention. The Canvas-side and Prism-side impacts are consequences of
> this decision, cross-referenced, not separate ADRs (they carry no independent
> architectural choice — they mirror the wire shape Orion defines here).

## 1. Context

ADR 002 (Pulsar) Amendment 1 selects the **maximal** live-test path: a *rich*
scene = **N distinct Blue blueprints** bound to distinct components. Conduit's
contract investigation found the wall this hits.

The current push envelope carries **exactly one** blueprint:

```go
// Orion/internal/compiler/types.go
type PushEnvelope struct {
    CanvasVersion   string         `json:"canvas_version,omitempty"`
    BlueBlueprintID string         `json:"blue_blueprint_id,omitempty"` // SINGULAR
    Components      []ComponentRef `json:"components,omitempty"`
    LSMLBundleHash  string         `json:"lsml_bundle_hash,omitempty"`
    RollbackTo      string         `json:"rollback_to,omitempty"`
}
```

`compile.go:55-63` fetches **one** blueprint from that single id (or skips on
`""`/`"none"`, ADR 007 §8 / issue #28), runs `validateBlueprint` over its nodes,
and topologically sorts a **single** `BlueprintGraph`. The producers mirror the
singular field: `ZabCanvas/.../orion_client.py::_build_envelope` takes one
`blue_blueprint_id`, and Prism emits one. There is **no notion of "which
component reads which blueprint"** — today the (single) blueprint is the scene's
one logic graph; the layout binds to its leaves by path.

To author a scene where component A is driven by blueprint X and component B by a
**distinct** blueprint Y, the envelope, the compiler, the client, and the
component↔blueprint binding all need to learn the plural. That is the decision
this ADR makes.

### 1.1 Cartography of the singular assumption (verified in code)

| Site | File | Singular assumption |
|---|---|---|
| Wire struct | `internal/compiler/types.go:11` | `BlueBlueprintID string` |
| Fetch + skip | `internal/compiler/compile.go:55-63` | one `FetchBlueprint(ctx, bpID)` |
| Validate | `internal/compiler/compile.go:111` (`validateBlueprint`) | one `*BlueprintGraph` |
| Topo sort | `internal/compiler/compile.go:119` | one `blueprint.Edges` set |
| Fetcher iface | `internal/compiler/http_fetcher.go` (`FetchBlueprint`) | one id → one graph |
| Canvas client | `ZabCanvas/.../orion_client.py:100-135` (`_build_envelope`) | `blue_blueprint_id: str` |
| Prism producer | `Prism/src/main/...` (push chain) | emits one id |

There is also a **second, subtler** gap: even with N graphs fetched, the compiler
must know **which component instance consumes which blueprint's leaves**, or the N
graphs collapse into one undifferentiated state namespace and the "distinct
blueprints" semantics are lost. §3.3 addresses this binding.

## 2. Decision drivers

- **Don't break the singular producers on day one.** Prism/Canvas at HEAD emit
  the singular field; a flag-day rename strands every existing scene and every
  not-yet-updated producer. Back-compat is mandatory, not optional.
- **One canonical internal shape.** The compiler should walk a **list** of
  blueprints uniformly; the singular case is "a list of length ≤ 1". No `if
  singular … else plural` branching in the compile core.
- **Deterministic hashing preserved.** `scene_version` is a content hash
  (`computeSceneVersion`, canonical JSON). The N-blueprint shape must hash
  deterministically — stable ordering of the blueprint list and of each graph's
  nodes/edges — or `scene_version`/the LSML adopt-on-verify (ADR 007 §C.4) breaks.
- **The component↔blueprint binding must be explicit and authored,** not inferred
  by name collision. Two blueprints may declare overlapping leaf names; the scene
  must say *this component reads that blueprint*.
- **Purity/cycle guarantees hold per-blueprint and across the union.** Criteria 17
  (cyclic component) / 18 (impure compute) must still reject; a cycle must not be
  able to hide by splitting across two blueprints that reference each other.

## 3. Decision

**Go.** Extend the push envelope from a single `blue_blueprint_id` to a **list of
blueprint references**, each with a stable local **key** used to bind components to
blueprints. The compiler internalises a `[]*BlueprintGraph` (the singular case =
length-1 list). Back-compat is achieved by **accepting both** the old singular
field and the new list on the wire, normalised to the list internally. No envelope
version bump — the change is **additive at the wire**, breaking only **internally**
(compiler signatures), which no external producer depends on.

### 3.1 Wire shape — new field `blueprints[]`, old field retained

Add a new optional field; **keep** `blue_blueprint_id` for back-compat:

```go
type PushEnvelope struct {
    CanvasVersion   string         `json:"canvas_version,omitempty"`

    // DEPRECATED but ACCEPTED: the legacy single blueprint. When set and
    // Blueprints is empty, the API layer normalises it into a one-element
    // Blueprints list with key "" (the default/anonymous blueprint key).
    // Emitted by Prism/Canvas before they adopt `blueprints`. Never both.
    BlueBlueprintID string         `json:"blue_blueprint_id,omitempty"`

    // NEW: the N distinct blueprints this scene binds. Each entry pairs a
    // Blue blueprint id with a scene-local Key that components reference to
    // declare which blueprint they consume (§3.3). Order is normalised
    // (sort by Key) before hashing for scene_version determinism.
    Blueprints      []BlueprintRef `json:"blueprints,omitempty"`

    Components      []ComponentRef `json:"components,omitempty"`
    LSMLBundleHash  string         `json:"lsml_bundle_hash,omitempty"`
    RollbackTo      string         `json:"rollback_to,omitempty"`
}

// BlueprintRef pairs a Blue blueprint id with the scene-local key that
// components use to bind to it. Key is unique within the envelope.
type BlueprintRef struct {
    Key string `json:"key"`           // scene-local handle, e.g. "score", "timer"
    ID  string `json:"id"`            // Blue blueprint id (the FetchBlueprint arg)
}
```

**Mutual-exclusion + normalisation rule** (enforced in the API layer
`scenes_push.go`, before the struct reaches `Compile`):

| `blue_blueprint_id` | `blueprints[]` | Normalised internal list |
|---|---|---|
| `""` / `"none"` / absent | empty | `[]` (blueprint-free scene, ADR 007 §8 — unchanged) |
| set | empty | `[{Key:"", ID:<that id>}]` (legacy single, key = `""`) |
| absent | non-empty | the list verbatim |
| **set** | **non-empty** | **400 `ENVELOPE_BLUEPRINT_CONFLICT`** — never both |

This means **no envelope version bump and no migration of stored data**: an old
producer keeps sending `blue_blueprint_id`, gets normalised to a length-1 list,
and compiles exactly as before. A new producer sends `blueprints[]`. The wire
stays one schema that reads both.

### 3.2 Compiler — internalise `[]*BlueprintGraph`, validate the union

`Compile` changes from one blueprint to a keyed set. The signature-bearing changes:

| Step | Before | After |
|---|---|---|
| Fetch | `FetchBlueprint(ctx, bpID)` once | loop over normalised `Blueprints`; `FetchBlueprint(ctx, ref.ID)` each, keyed by `ref.Key` into `map[string]*BlueprintGraph` |
| Fetcher iface | `FetchBlueprint(ctx, id) (*BlueprintGraph, error)` | **unchanged** — still one id → one graph; the loop is the caller's |
| Validate | `validateBlueprint(blueprint, manifest)` | `validateBlueprint(bp, manifest)` **per graph**; results merged into the runtime node set, **each node's leaf path prefixed by its blueprint key** (§3.3) |
| Cycle/topo | `topologicalSort(nodes, blueprint.Edges)` | sort **per-blueprint** (edges are intra-blueprint; v1 forbids cross-blueprint edges — §3.4), then concatenate in stable key order |
| Purity (crit 18) | per node | **unchanged** — applies to every node across all graphs |
| Empty case | `&BlueprintGraph{}` when `""`/`"none"` | normalised list is `[]`; the per-graph loop runs zero times → same zero-value behaviour. Issue #28 robustness preserved. |

The compile core stays single-pass. The only structural change is the **loop** and
the **key-prefixing** of leaf paths.

### 3.3 The component↔blueprint binding — leaf-path namespacing by key

The load-bearing semantic: with N blueprints, two of them may both declare a
`core.output@1` named `"value"`. Today (single blueprint) the leaf is just
`value`. With N blueprints these would **collide in one state namespace**. The
binding rule:

1. Each blueprint in the envelope has a unique scene-local **`key`** (§3.1).
2. The compiler **prefixes every leaf path** a blueprint contributes with
   `<key>.` (e.g. blueprint key `score` → leaf `value` becomes `score.value`).
   The legacy length-1 list uses key `""`, whose prefix is empty → **byte-identical
   leaf paths to today** (back-compat for stored single-blueprint scenes).
3. A **component declares which blueprint it reads** via its layout binding
   addressing the keyed leaf: a `LayoutNode.Bindings` value of `score.value` reads
   blueprint `score`'s output `value`. No new struct field on `LayoutNode` is
   required — the existing `Bindings map[string]string` already carries dotted
   state paths; the key is just the new leading segment. **This keeps the
   `CanvasLayout`/`LayoutNode` wire unchanged** and pushes the binding into the
   authored path string, which Canvas/Prism already own.
4. The compiler **validates** that every binding's leading segment names a declared
   blueprint key (or is keyless for a non-blueprint binding) and emits
   `UNKNOWN_BLUEPRINT_KEY` (new error code) on a dangling reference — so a typo'd
   key fails the push instead of silently reading nothing.

> **Authoring-side note (Canvas/Prism, consequence not decision):** the editor must
> let the author pick *which blueprint key* a component's binding targets, and emit
> the `<key>.<leaf>` path. This is a Prism/Canvas editor concern tracked as a
> follow-up; for **M8** the fixture is a checked-in LSML bundle authoring the
> `<key>.<leaf>` paths by hand, so it does not block on the editor UI.

### 3.4 Scope boundaries (v1)

- **No cross-blueprint edges.** A `BlueprintEdge` connects nodes **within one
  blueprint**. Wiring blueprint X's output into blueprint Y's input is **out of
  scope** for v1 — the composition point is the **component layer** (a component
  reads X's leaf, another reads Y's), not the blueprint edge graph. This keeps
  per-blueprint topo-sort independent and cycle detection tractable. A future ADR
  can add cross-graph edges if a real need appears.
- **Cycle detection** (criterion 17, component-uses-component) is unchanged — it
  already runs over the component graph, which is orthogonal to the blueprint set.
  Each blueprint's internal edge cycle is still rejected by its own `topologicalSort`.
- **Manifest fetch** (`FetchComputeManifest`) stays **one call** — the manifest is
  global to Blue, shared across all blueprints. No per-blueprint manifest.

### 3.5 Hashing / determinism

- Before hashing, the API layer **sorts `Blueprints` by `Key`** and the compiler
  emits runtime nodes in `(key, topo-order)` order. `computeSceneVersion`'s
  canonical JSON then yields a stable `scene_version` for identical inputs
  regardless of the authored order of `blueprints[]`.
- The LSML adopt-on-verify path (ADR 007 §C.4, `LSMLBundleHash`) is **unaffected
  in shape** but its inputs grow; the cross-language golden (Orion issue #18) must
  be extended to cover an N-blueprint bundle so Go and TS agree on the hash.

## 4. Consequences

- The platform can author scenes with **independent logic graphs per component** —
  the real "rich scene" the maximal M8 path wants, and a capability the editor can
  expose generally.
- **Zero migration, zero flag-day.** Old producers keep working via the singular
  field; stored single-blueprint scenes hash identically (empty key prefix).
- The compiler gains a loop + a key-prefix; the **fetcher interface is untouched**,
  so the HTTP fetcher and its tests change minimally.
- A new error class (`UNKNOWN_BLUEPRINT_KEY`, `ENVELOPE_BLUEPRINT_CONFLICT`) — both
  testable, both fail-closed.
- Canvas's `_build_envelope` and Prism's producer grow a `blueprints` path while
  **keeping** the singular path; the Canvas↔Orion contract test (Orion issue #27)
  must cover both shapes.
- Debt retired eventually: once all producers emit `blueprints[]`, a later ADR can
  deprecate-and-remove `blue_blueprint_id`. Not now.

## 5. Risks

Security-surfaced risks → **Bastion** (do not self-clear).

- **R1 — Leaf-path collision across blueprints (correctness, not security).**
  Mitigated by the mandatory `<key>.` prefix (§3.3); a missing/duplicate key is a
  hard error (`ENVELOPE_BLUEPRINT_CONFLICT` on dup keys, `UNKNOWN_BLUEPRINT_KEY` on
  dangling). Residual: an author could bind to the wrong key and read the wrong
  blueprint's value silently — caught by the scene's own provenance assertions, not
  by the compiler. Accepted (authoring error, not a contract break).
- **R2 — Hash instability if ordering is not normalised.** Mitigated by the §3.5
  sort-before-hash rule; gated by the extended cross-language golden (issue #18).
  **Must be in the test suite before merge**, or `scene_version` drifts between
  producers.
- **R3 — Fan-out fetch amplification.** N blueprints = N `FetchBlueprint` calls per
  push. Bounded by the authored blueprint count (small, human-authored); HTTP
  keepalive amortises (compile.go's existing serial-but-keepalive note). No
  unbounded fan-out. → Bastion: confirm no DoS surface (push is operator-only,
  already gated).
- **R4 — Back-compat regression.** The singular→list normalisation must be
  byte-exact for the length-1/empty-key case or every existing scene re-hashes.
  Covered by a golden test asserting a singular-field push and the equivalent
  `blueprints:[{key:"",id:X}]` push produce the **same `scene_version`**.

No new auth primitive, no new network surface (same operator-gated push endpoint).

## 6. Resolution criteria

Testable, aligned with Orion's CLAUDE.md gates and the M-series convention.

1. **Wire back-compat.** A push with the legacy `blue_blueprint_id` (and no
   `blueprints`) compiles to the **same `scene_version`** as the equivalent
   `blueprints:[{key:"",id:X}]` push (golden test). An old producer is unaffected.
2. **Conflict rejected.** A push with **both** `blue_blueprint_id` and
   `blueprints[]` returns `400 ENVELOPE_BLUEPRINT_CONFLICT`.
3. **N-blueprint compile.** A push with `blueprints[]` of length ≥ 2, distinct ids,
   distinct keys, compiles with no `COMPILE_FAILED`; each blueprint's leaves appear
   in the runtime state under its `<key>.` prefix (asserted on the graph).
4. **Binding validation.** A component binding referencing an undeclared key fails
   with `UNKNOWN_BLUEPRINT_KEY`; a correct `<key>.<leaf>` binding resolves to that
   blueprint's leaf.
5. **Determinism.** Re-ordering `blueprints[]` in the request yields the **same**
   `scene_version` (sort-before-hash, §3.5).
6. **Per-blueprint purity/cycle.** An impure compute in **any** blueprint →
   `IMPURE_COMPUTE`; an edge cycle in any blueprint → topology error. Criteria
   17/18 hold across the union.
7. **Cross-language hash.** The Orion issue #18 golden is extended to an
   N-blueprint bundle; Go (`lumencast-go`) and TS (`@lumencast/compiler`) agree.
8. **Canvas client contract.** `ZabCanvas/.../orion_client.py::_build_envelope`
   emits `blueprints[]` when given multiple, **and** still emits the singular field
   for the one-blueprint legacy path; the Canvas↔Orion contract test (issue #27)
   covers both. `mypy --strict` + `ruff` green.
9. **Org gates.** Orion CI green (vet/test/build/staticcheck/golangci/trufflehog);
   Canvas CI green (ruff/mypy/pytest); review approved by Vigil; Conduit validates
   the cross-service contract; Bastion clearance on R3 (no new surface).
