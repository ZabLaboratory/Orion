# Embedded-local frozen contracts (ADR 016 §B) — Conduit

> Conduit-finalized, for Forge **#224** (`bundledFetcher`) and **#225** (local
> data sidecar). Proven against BOTH sides of the live code (producer +
> consumer), 2026-06-21. Refs ADR 016 §B-3 / §B-4, RC-5. Closes #221.
>
> This doc fixes the exact shapes the local sidecars must serve so the **hot
> path stays BIT-IDENTICAL between antenna and embedded-local**. It implements
> nothing — Forge builds the sidecars to this contract; every diff is
> Vigil-reviewed in its repo, Bastion-cleared if it touches the trust surface,
> and Eleven decides the merge order. NOT self-merged.

## Profile recap (why this exists)

In `embedded-local` Orion runs the **real** engine; its remote dependencies
collapse into loopback sidecars bundled inside Prism. Two hot-path I/O contracts
must be reproduced byte-for-byte by those sidecars:

1. **`_query`** — the local data sidecar (#225, SQLite mirrors of truth/ranking)
   must answer `POST <base>/<svc>/api/v1/_query` exactly as ZabGate→ZabTruth/
   ZabRanking answers it today. Consumer: `internal/effects/dbquery.go`.
2. **Bundle fetch** — the `bundledFetcher` (#224) must return the frozen scene's
   artefacts in the exact shapes `internal/compiler.Fetcher` already decodes.
   Consumer: `internal/compiler/fetcher.go` + `http_fetcher.go`.

The single rule for both: **the sidecar substitutes the transport, never the
shape.** Orion's decode path is unchanged; if a byte differs, the hot path is
no longer identical and the profile is broken.

---

## Contract A — `_query` (local == wire ZabGate)

### A.1 Endpoint

`POST <base>/<svc>/api/v1/_query`, where `<base>` is the loopback gateway URL
(`DBQueryClient.gatewayURL`, e.g. `http://127.0.0.1:<port>`) and `<svc>` is the
ZabGate prefix of the owning service (`truth`, `ranking`). Emitted by
`DBQueryClient.Query` (`dbquery.go:110`):

```
u := fmt.Sprintf("%s/%s/api/v1/_query", c.gatewayURL, ds.Svc)
```

The local sidecar therefore serves **the path with the `<svc>` segment intact**
(no ZabGate prefix-strip in embedded-local — `<base>` already points at the
sidecar; the `/<svc>/api/v1/_query` suffix is the full path Orion writes).

### A.2 Request

| Aspect | Value (frozen) | Source |
|---|---|---|
| Method | `POST` | `dbquery.go:111` |
| `Content-Type` | `application/json` | `dbquery.go:115` |
| `Authorization` | `Bearer <service-token>` when token non-empty, **absent otherwise** | `dbquery.go:116-118` |
| Body | QueryMe `QueryDescriptor` JSON, `extra="forbid"`, passed verbatim | `QueryMe/src/queryme/descriptor.py` |

`QueryDescriptor` shape (canonical `from → (where|join)* → select → (order|limit)?`):

```json
{
  "table": "players",
  "where":  [{"column": "team", "op": "=", "value": "ZAB"}],
  "joins":  [{"table": "matches", "on": ["match_id", "id"], "select": ["patch"]}],
  "select": ["summoner_name", "role"],
  "order":  [{"column": "summoner_name", "direction": "asc"}],
  "limit": 50,
  "offset": 0
}
```

Operators (closed list, `descriptor.py:29`): `= != > < >= <= IN LIKE "IS NULL"`.
`IN` ⇒ list value; `LIKE` ⇒ string value; `IS NULL` ignores value. Order
directions: `asc | desc`.

### A.3 Response — 200 (the bit-identity surface)

```json
{"rows": [ {"<col>": <scalar>, ...}, ... ], "count": <int>, "elapsed_ms": <number>}
```

Decoded by Orion into `QueryResult` (`dbquery.go:57-62`):

| Field | Orion Go type | Service emits | Note |
|---|---|---|---|
| `rows` | `json.RawMessage` (opaque) | `list[dict]` | one dict per row, keyed by output column |
| `count` | `int` | `len(out_rows)` | |
| `elapsed_ms` | `float64` | `int(...)` (ZabTruth/ZabRanking) | int decodes into float64 — JSON-compatible |

**Row key derivation (must match exactly).** Keys = `descriptor.select` then,
per join in order, `join.select`. On a name collision the whole key list is
rebuilt qualifying the *joined* duplicate as `"<join.table>.<col>"` (the FROM
column keeps its bare name). Verbatim algorithm:
`ZabTruth/.../routes/internal.py:104-117` (identical in ZabRanking).

**Scalar serialisation (must match exactly).** `_serialise`
(`internal.py:56-70`, identical in ZabRanking):

| DB value | JSON form |
|---|---|
| `UUID` | string (`str(value)`) |
| `Decimal` | float (`float(value)`) |
| `datetime` / `date` | ISO-8601 string (`.isoformat()`) |
| bool / int / float / str / None | passthrough |

### A.4 Errors

| Status | Body | Trigger |
|---|---|---|
| `400` | `{"detail": {"issues": [<issue>, ...]}}` | validation OR compilation failure (`validate_against_schema` / `CompilationError`) |
| `403` | `{"error": "MISSING_QUERY_SCOPE", "required": "query.read.<svc>"}` | service-token caller lacking the scope |

Orion's consumer treats any non-200 as an error string for the effect's `error`
port (`dbquery.go:128-129`): `_query <name>: status <code>: <body>`. The 400
body is surfaced verbatim — the local sidecar **must** keep the
`{"detail":{"issues":[...]}}` envelope for the editor/error-port to read
(test `TestDBQuery_400CarriesIssues` asserts the substring `issues`).

### A.5 Auth in embedded-local

ZabGate's role/scope injection does not exist on loopback. Per ADR 016 §B the
sidecar runs **inside Prism's trust boundary** (loopback only, not network-
reachable). Two valid stances, Forge picks one and states it in #225:

- **(a) no-auth loopback** — sidecar ignores `Authorization`, serves every
  `_query`. Matches Orion's `tokenFn == ""` path (no header sent). Simplest;
  acceptable because the boundary is the process, not the token.
- **(b) parity stub** — sidecar honours the same `X-Authenticated-Paths` /
  `query.read.<svc>` 403 shape for fidelity. Only needed if a local test must
  exercise the scope-denial path.

Either way the **200 path is byte-identical** — that is the hot-path requirement.
This is a Bastion touch-point (auth surface) → clearance before #225 merges.

### A.6 Non-parity risks the SQLite mirror MUST handle

The owning services run PostgreSQL via SQLAlchemy + QueryMe's `compile_query`.
A SQLite mirror is iso on the wire ONLY if it reproduces these, else the hot
path silently diverges:

1. **Scalar coercion** — SQLite has no native `UUID`/`Decimal`/`datetime`
   types; if rows come back as raw strings the JSON already matches `_serialise`
   output *for those cases*, but a `Decimal` column stored as REAL must emit a
   JSON **float**, and a UUID stored as TEXT must emit the **canonical lowercase
   hex string** (no braces). Mirror the `_serialise` table above explicitly.
2. **`ILIKE` vs `LIKE`** — QueryMe leaves case-sensitivity to the service
   (`descriptor.py:27-28`). Postgres services may compile `LIKE`→`ILIKE`; SQLite
   `LIKE` is case-insensitive for ASCII by default but **case-sensitive after a
   `PRAGMA case_sensitive_like`**. Match whatever the truth/ranking service does
   per column, or rows differ.
3. **Numeric typing** — Postgres `Numeric(4,2)` (e.g. ZabRanking `player_scores.score`)
   → `Decimal` → JSON float. SQLite must not emit it as a string.
4. **ORDER BY collation / NULL ordering** — Postgres sorts NULLs last on `asc`
   by default; SQLite sorts NULLs **first**. Any `order` over a nullable column
   reorders rows ⇒ non-identical `rows[]`. The mirror must apply Postgres NULL
   ordering (`ORDER BY col IS NULL, col` for asc) where the source column is
   nullable.
5. **No pagination drift** — `limit`/`offset` are passed through; identical SQL
   semantics. No risk, listed for completeness.

Items 1–4 are **the** parity work of #225. A golden contract test
(`internal/effects/dbquery_local_contract_test.go`, added by this PR) locks the
exact 200 envelope shape Orion decodes so the sidecar has an executable target.

---

## Contract B — frozen scene bundle (bundledFetcher #224)

`bundledFetcher` implements `internal/compiler.Fetcher` (`fetcher.go:13-43`)
against a frozen, on-disk bundle instead of HTTP. The compiler is unchanged: it
calls the same five methods and decodes the same Go structs. The bundle for the
embedded-local default scene = the **canvas-chat-sponso** layout + its **5
materialised blueprints**.

### B.1 `FetchCanvasLayout(ctx, canvasVersion) (*CanvasLayout, error)`

Production: `GET {canvas}/api/v1/layouts/{version}` → `CanvasLayout` JSON
(`http_fetcher.go:65-73`; producer `ZabCanvas/.../routes/layouts.py`). The
`{version}` is the bare sha256-64hex content address (`PushEnvelope.CanvasVersion`).

Frozen form: the bundle stores, keyed by that exact 64-hex version, a JSON object
decodable into `compiler.CanvasLayout` (`internal/compiler/types.go:71-101`):

```jsonc
{
  "version": "<64-hex>",          // == the key, == envelope.CanvasVersion
  "root": { /* LayoutNode tree */ },
  "operator_inputs": [ /* OperatorInput, omitempty */ ],
  "animations": { /* opaque, omitempty — forwarded verbatim */ },
  "assets":     { /* {allowedHosts[],fonts[],preload[]}, opaque, omitempty */ }
}
```

`LayoutNode` (`types.go:106-164`): `kind`, `id?`, `props?` (`map[str]raw`),
`bindings?` (`map[str]str`), `transitions?`, `children?`, `component_args?`.
`animations` and `assets` are opaque `json.RawMessage` — store the **exact
bytes ZabCanvas served** (they ride into the LSML bundle and feed the Solar
host allowlist; reshaping them breaks the C4 LSML content hash and the runtime
double-gate). `keyframes`/`animate_initial` on nodes are render-bundle-only,
produced by Orion's lowering — they are **never** authored in this fetched
layout, so the frozen layout must not carry them (parity with the live fetch).

> Freeze recipe (#224): capture the live `GET /canvas/api/v1/layouts/<ver>`
> 200 body verbatim into the bundle under key `<ver>`. No transform.

> **⚠ AMENDMENT 2 (2026-06-21, e2e #152 defect 2) — `assets.allowedHosts` must
> be present.** The LSML authoring gate (`authoring_gate.go::checkAssetURL`, T1)
> rejects any remote asset whose parsed host is not in `assets.allowedHosts`; an
> absent/empty block denies every remote host (deny-by-default) → a 422 on push.
> The frozen canvas-chat-sponso layout was captured WITHOUT an `assets` block
> while referencing remote hosts (`ddragon.leagueoflegends.com` champion
> portraits via `pl.*.champ` operator-input defaults; `www.figma.com` MCP
> placeholders in `<image src>`). When ZabCanvas omits the block, the freeze
> step derives `allowedHosts` from the hosts the layout actually references (tree
> + operator-input defaults; exact-match, never a wildcard).
> **`www.figma.com` is an authoring leak** — ephemeral design-to-code URLs that
> expire/401; allowlisting only unblocks the gate. Re-pointing those 3 portrait
> assets to a durable host is an AUTHORING follow-up (ZabCanvas layout re-gen /
> Forge), not a wiring fix — the freeze script emits a loud warning.

### B.2 `FetchBlueprint(ctx, blueprintID) (*BlueprintGraph, error)`

Production is a **TWO-CALL** fetch (`http_fetcher.go:87-106`):
1. `GET {blue}/api/v1/blueprints/{id}` → reads `current_version` (int) off the
   blueprint row.
2. `GET {blue}/api/v1/blueprints/{id}/versions/{current_version}` → lifts nested
   `graph.{nodes,edges}` into the flat `BlueprintGraph`.

Frozen form: the bundle resolves an id directly to its `BlueprintGraph`
(`types.go:222-252`) — the sidecar collapses the two calls into one local
lookup, returning:

```jsonc
{
  "id": "<blueprint-uuid>",
  "nodes": [ /* BlueprintNode */ ],
  "edges": [ /* BlueprintEdge */ ],
  "variables": [ /* BlueprintVariable, omitempty */ ]
}
```

`BlueprintNode` (`types.go:363-378`): `id`, `definition` (the qualified
`namespace.name@version` — **wire field is `definition`, not `compute`**),
`config?`, `inputs?`, `outputs?`, `reference?`. `BlueprintEdge`
(`types.go:409-414`): snake_case `from_node`/`from_port`/`to_node`/`to_port`.
`BlueprintVariable` (`types.go:286-291`): `id`,`name`,`type`,`value?`.

> Freeze recipe: for the scene's default-version blueprints, capture the lifted
> `{nodes,edges,variables}` (i.e. the `graph` block of the published
> `current_version`) under the blueprint id.

> **⚠ AMENDMENT 1 (2026-06-21, e2e #152 defect 1) — node ports MUST be baked.**
> Blue STORES authoring graphs whose nodes carry EMPTY `inputs`/`outputs`. Both
> `GET /versions/{v}` AND `GET /versions/{v}/graph` serve them empty (verified
> live against `34f4b958` v5: 316 nodes, 0 ports on every node, on both
> endpoints). The per-port specs — crucially the `data`|`exec` discriminator
> Orion's exec partition reads (`exec_partition.go::isExecNode` keys off
> `BlueprintPort.Kind`) — live ONLY on the node-definition signature
> (`GET /node-definitions`), NOT on the compute manifest (which carries
> declared-input *names* but no `kind`). On the live antenna path the editor
> hydrates node ports from `def.signature`; a verbatim graph capture loses
> them, so the exec partition builds 0 programs and no on-call entrypoint arms.
>
> **Therefore the freeze step MUST bake each non-`reference` node's
> `inputs`/`outputs` from its node-definition signature** (keyed by
> `definition`), projecting `name`/`type`/`kind`/`required`/`default` onto
> `BlueprintPort`. `reference` nodes are skipped (their pins come from the
> resolved interface). The `bundledFetcher` does NOT enrich — it returns frozen
> graphs verbatim — so the ports must already be on disk. Producer:
> `Prism/scripts/build-scene-bundle.mjs` (port bake from
> `node_definitions.json`). Proof: `frozen_bundle_exec_ports_test.go`
> (portless on-call arms nothing; baked on-call arms the entrypoint).

### B.3 `FetchBlueprintGraph(ctx, blueprintID, version) (*ResolvedBlueprintGraph, error)`

Production: `GET {blue}/api/v1/blueprints/{id}/versions/{version}/graph` —
PINNED, published-only (Blue producer `routes/versions.py:122-189`,
`ResolvedGraph`). Used for ADR 014 reference expansion. Decoded into
`ResolvedBlueprintGraph` (`types.go:261-278`):

```jsonc
{
  "blueprint_id": "<uuid>",
  "version": <int>,
  "nodes": [ /* BlueprintNode */ ],
  "edges": [ /* BlueprintEdge */ ],
  "variables": [ /* BlueprintVariable, omitempty */ ],
  "interface": { "inputs": [ /* pin */ ], "outputs": [ /* pin */ ] },
  "purity": { "is_pure": <bool>, "is_bounded": <bool> }
}
```

Pin (`BlueprintInterfacePin`, `types.go:315-320`): `name`,`type`,`kind?`
(`data`|`exec`, empty⇒data),`required`. Blue serves `status` too; Orion ignores
it (not in the struct) — harmless if present in the frozen blob.

**Pinned-resolution errors must be reproduced** (the 5 materialised blueprints
may reference each other): if a referenced `(id, version)` is absent/unpublished
in the bundle, the sidecar returns a typed body Orion can map to
`BLUEPRINT_REF_UNRESOLVED` — i.e. a `404`/`422` whose JSON top-level `code` is one
of `BLUEPRINT_NOT_FOUND` / `BLUEPRINT_VERSION_NOT_FOUND` /
`BLUEPRINT_VERSION_NOT_PUBLISHED` (`http_fetcher.go:113-156`, `blueRefUnresolvedCodes`).
A frozen bundle that materialises every referenced version will never hit this —
but a missing artefact must fail closed, **never** silently substitute another
version (Blue `docs/contracts/graph-resolution.md`).

### B.4 `FetchComponent` / `FetchComputeManifest`

- `FetchComponent(ctx, ref)` → `GET {canvas}/api/v1/components/{id}/{version}` →
  `UserComponent` (`types.go:206-212`: `id`,`version`,`parameters`,`body`,
  `operator_inputs?`). Frozen: keyed by `(id,version)`. The canvas-chat-sponso
  scene uses no user components ⇒ **#224 may serve none** (any `FetchComponent`
  call is then a bundle-miss = a hard fetch error, which is correct: the frozen
  scene declares no components).
- `FetchComputeManifest(ctx)` → `GET {blue}/api/v1/_compute-manifest` →
  envelope `{"entries":[...],"count":N}` adapted into `ComputeManifest`
  (`http_fetcher.go:168-204`). Each `entry` (`blueManifestEntry`, `types.go:448-459`):
  `node_id` (`namespace.name@version`), `is_pure`, `is_bounded`,
  `declared_inputs` (list of dicts), `declared_output_type` (str | list | null),
  `version` (int). Frozen: capture the live `_compute-manifest` 200 body verbatim;
  it must contain every `definition` the 5 blueprints reference, or the compiler
  rejects the node (`UNKNOWN_COMPUTE_NODE`). The manifest is **deploy-constant**,
  so a verbatim snapshot is sufficient and stable.

### B.5 Bundle freeze invariant

Capture all artefacts **verbatim from the live 200 bodies**, then collapse the
2-call `FetchBlueprint` into a single id→graph lookup. The compiler never knows
the difference: same structs, same bytes. A golden test
(`internal/compiler/bundled_fetcher_contract_test.go`, added by this PR) decodes
representative frozen blobs through the real `compiler` structs to lock the
shapes #224 must produce.

---

## Verdict & green light

**Aligned.** Both contracts are derived from the live producer **and** consumer
code, not from memory. The hot-path 200 surfaces are unambiguous and locked by
golden tests in this PR.

- **#224 (bundledFetcher)** — UNBLOCKED. Implement `compiler.Fetcher` against a
  frozen bundle per Contract B; collapse `FetchBlueprint`'s two calls locally;
  reproduce the pinned-resolution typed errors (B.3) fail-closed.
- **#225 (data sidecar)** — UNBLOCKED with one Bastion touch-point (A.5 auth
  stance) and the four mandatory parity items (A.6 #1-4: scalar coercion, LIKE
  collation, numeric typing, NULL ordering). These are SQLite↔Postgres semantic
  gaps, not contract gaps — the wire shape is fully specified.

No breaking change to any live antenna contract: this is additive (a new
transport behind the existing `Fetcher` / `_query` shapes). Eleven decides merge
order; producer-before-consumer does not apply (no shared schema changes — the
sidecars consume the frozen shapes, they do not alter them).
