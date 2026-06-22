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

---

## Contract C — `/canvas` + `/blue` loopback (ADR 016 Amendment 1)

> Conduit-finalized for **issue #245** (ADR 016 Amendment 1, scene-agnostic
> embedded-local). Proven against BOTH live sides on `origin/main`, 2026-06-22:
> consumer = Orion `internal/compiler/{http_fetcher,fetcher,types}.go`; producers
> = ZabCanvas `src/zabcanvas/routes/{layouts,user_components}.py` + Blue
> `src/blue/routes/{blueprints,versions,compute_manifest,node_definitions}.py`
> and `src/blue/schemas/{graph,version,blueprint}.py`. NOT self-merged: Eleven
> decides the merge per the merge gate (`docs/rules/git.md`).
>
> **Why this exists.** Amendment 1 reverses §3.2(4): embedded-local stops using
> `bundledFetcher` (one frozen scene) and re-uses the **antenna `httpFetcher`**
> (`http_fetcher.go`) pointed at a **loopback gateway sidecar** bundled in Prism,
> exactly as the antenna points it at ZabGate→ZabCanvas/Blue. Scene-agnostic in
> local needs no new engine mechanism — only that the sidecar serve, byte-for-byte,
> the same routes the `httpFetcher` already calls. The single rule of §B holds
> here too: **substitute the transport, never the shape.**

### C.0 Base URLs & path discipline

In embedded-local Prism wires Orion's three base URLs at the loopback sidecar:

| Orion config | Antenna value | Embedded-local value |
|---|---|---|
| `ORION_CANVAS_BASE_URL` (`CanvasBase`) | `http://zabgate:4000/canvas` | `http://127.0.0.1:<port>/canvas` |
| `ORION_BLUE_BASE_URL` (`BlueBase`) | `http://zabgate:4000/blue` | `http://127.0.0.1:<port>/blue` |
| `ORION_ZABGATE_URL` (`gatewayURL`) | `http://zabgate:4000` | `http://127.0.0.1:<port>` |

The `httpFetcher` writes the **full `/api/v1/...` suffix** onto `CanvasBase` /
`BlueBase` (`http_fetcher.go:68,92,101,136,161,182`). **There is no `/canvas` or
`/blue` prefix strip** like ZabGate does for `_query`'s `<svc>` segment — whatever
`CanvasBase`/`BlueBase` already carry is the full origin, and Orion appends
`/api/v1/...`. The sidecar may serve one base routed by path or two bases; the
contract below is **per absolute path the fetcher emits** (the `{canvas}`/`{blue}`
token = the configured base, e.g. `…/canvas` or `…/blue`).

Auth on loopback: ZabGate's `X-Authenticated-*` injection does not exist here.
The `httpFetcher` sends `Authorization: Bearer <token>` when `TokenFunc` is
non-nil, else anonymous (`http_fetcher.go:265-271`). Sidecar auth stance =
same two valid options as `_query` §A.5 (no-auth loopback, or parity stub); the
**200 body is byte-identical either way**. Bastion touch-point (loopback auth
surface) → clearance before the sidecar merges (#163), same as #225.

### C.1 — `R1` `GET {canvas}/api/v1/layouts/{version}` → `CanvasLayout`

Consumer `FetchCanvasLayout` (`http_fetcher.go:65-73`). Producer ZabCanvas
`routes/layouts.py:51` (`get_layout_endpoint`, mounted `prefix=/api/v1`), serving
`adapt_bundle_to_layout` (`services/layout_adapter.py`). `{version}` is the bare
sha256 64-hex content address, **producer-validated** `^[0-9a-f]{64}$`
(`layouts.py::_HASH_PATTERN`) — a non-matching version is rejected upstream and
must be by the mirror too.

Decoded into `CanvasLayout` (`types.go:71-101`):

```jsonc
{
  "version": "<64-hex>",            // == the path key, == PushEnvelope.CanvasVersion
  "root": { /* LayoutNode tree */ },
  "operator_inputs": [ /* OperatorInput */ ]?,   // omitempty
  "animations": { /* opaque */ }?,               // omitempty — verbatim bytes
  "assets":     { "allowedHosts":[], "fonts":[], "preload":[] }?  // omitempty — verbatim bytes
}
```

`LayoutNode` (`types.go:106-164`): `kind`, `id?`, `props?` (`map[str]raw`),
`bindings?` (`map[str]str`), `transitions?`, `children?`, `component_args?`.
`animations`/`assets` are opaque `json.RawMessage` — the mirror stores the **exact
bytes ZabCanvas served** (they feed the C4 LSML content-hash and the Solar host
allowlist; reshaping breaks the runtime double-gate). `keyframes`/`animate_initial`
are render-bundle-only (Orion lowering) and must **never** appear on this fetched
layout. **`assets.allowedHosts` must be present when the layout references remote
hosts** or the LSML authoring gate 422s on push (Amendment 2, §B.1) — a property
of the published artefact the mirror captures, not a sidecar transform.

### C.2 — `R2`+`R3` `FetchBlueprint` (two-call)

Consumer `FetchBlueprint` (`http_fetcher.go:87-106`) is a **two-call** fetch:

- **R2** `GET {blue}/api/v1/blueprints/{id}` → Orion reads only `current_version`
  (int). Producer Blue `routes/blueprints.py:79` (`BlueprintRead`,
  `schemas/blueprint.py:46`, field `current_version:int` at `:58`). The mirror
  may serve the full `BlueprintRead` or the minimum `{ "id", "current_version" }`.
- **R3** `GET {blue}/api/v1/blueprints/{id}/versions/{current_version}` → Orion
  lifts nested `graph.{nodes,edges}`. Producer Blue `routes/versions.py:111`
  (`VersionRead`, `schemas/version.py:48`; router prefix
  `/blueprints/{blueprint_id}/versions`).

`VersionRead.graph` is a `Graph` (`schemas/graph.py:118`):

```jsonc
{ "graph": { "nodes": [ /* Node */ ], "edges": [ /* Edge */ ], "variables": [ /* Variable */ ] }, … }
```

`Node` (`graph.py:71`): `id`, `definition` (qualified `namespace.name@version` —
**wire field is `definition`, not `compute`**), `config?`, `inputs:[Port]`,
`outputs:[Port]`. `Port` (`graph.py:59`): `id`, `name`, `type`, `required`,
`default?` — **note: a graph `Port` has NO `kind` field** (see écart B). `Edge`
(`graph.py:90`): snake_case `from_node`/`from_port`/`to_node`/`to_port`.
`Variable` (`graph.py:107`): `id`, `name`, `type`, `value?`. These map onto
Orion's `BlueprintNode`/`BlueprintEdge`/`BlueprintVariable` (`types.go:220-291`).

### C.3 — `R4` `GET {blue}/api/v1/blueprints/{id}/versions/{version}/graph` → `ResolvedGraph`

Consumer `FetchBlueprintGraph` (`http_fetcher.go:127-156`): the **PINNED,
published-only** endpoint for ADR 014 reference expansion. Producer Blue
`routes/versions.py:130` (`response_model=ResolvedGraph`, `schemas/version.py:89`).
Decoded into `ResolvedBlueprintGraph` (`types.go:261-278`):

```jsonc
{
  "blueprint_id": "<uuid>",
  "version": <int>,
  "status": "published",          // always "published" on a 200; Orion ignores it
  "nodes": [ /* Node */ ],
  "edges": [ /* Edge */ ],
  "variables": [ /* Variable */ ],
  "interface": { "inputs": [ InterfacePort ], "outputs": [ InterfacePort ] },
  "purity": { "is_pure": <bool>, "is_bounded": <bool> }
}
```

`InterfacePort` (`graph.py:128`) carries `name`, `type`, `kind` (`data|exec`,
empty⇒data), `required` — **the interface is the only wire surface that carries
the exec/data discriminant** (see écart B).

**R4 typed errors — MANDATORY, fail-closed.** A referenced `(id, version)` absent
or unpublished in the mirror MUST return the same typed body the live Blue raises,
so Orion maps it to `BLUEPRINT_REF_UNRESOLVED` and rejects the push (never
substitutes another version). Producer `versions.py` `/graph` (`ResolveError`,
`version.py:121`):

| Status | top-level `code` | Trigger |
|---|---|---|
| `404` | `BLUEPRINT_NOT_FOUND` | blueprint id unknown |
| `404` | `BLUEPRINT_VERSION_NOT_FOUND` | version absent / non-existent |
| `422` | `BLUEPRINT_VERSION_NOT_PUBLISHED` | version exists but is a draft |

Body: `{ "code", "message", "blueprint_id", "version" }`. Orion's
`blueRefUnresolvedCodes` (`http_fetcher.go:113-156`) keys off the top-level `code`.
A mirror that materialises every referenced version never hits this — but a
**missing artefact must fail closed** (a 404 `BLUEPRINT_VERSION_NOT_FOUND` is the
correct route→file miss response), never a silent substitution.

### C.4 — `R5` `GET {blue}/api/v1/_compute-manifest` → `{entries,count}`

Consumer `FetchComputeManifest` (`http_fetcher.go:168-204`). Producer Blue
`routes/compute_manifest.py:79` (`ComputeManifestResponse`, mounted `/api/v1`):

```jsonc
{ "entries": [ ManifestEntryDTO ], "count": <int> }
```

`ManifestEntryDTO` (`compute_manifest.py:32`): `node_id` (`namespace.name@version`),
`is_pure`, `is_bounded`, `declared_inputs` (`list[dict]`), `declared_output_type`
(`str | list[str] | null`), `version` (int) — plus `namespace`/`name`/`source`/
`platform` which Orion ignores. The manifest MUST contain every `definition` the
scene's blueprints reference or the compiler rejects the node
(`UNKNOWN_COMPUTE_NODE`). Deploy-constant ⇒ a verbatim snapshot is sufficient.

### C.5 — `R6` `GET {canvas}/api/v1/components/{id}/{version}` → `UserComponent` (OPTIONAL — deferred)

Consumer `FetchComponent` (`http_fetcher.go:158-166`) calls
`/api/v1/components/{id}/{version}` → `UserComponent` (`types.go:206-212`):
`id`, `version`, `parameters[]`, `body{LayoutNode}`, `operator_inputs?`.

**R6 is OPTIONAL for the MVP and DEFERRED (Eleven decision A).** Two facts:
1. **Path mismatch (pre-existing antenna bug, see écart A):** Orion calls
   `/api/v1/components/{id}/{version}`, but ZabCanvas mounts NO `/components`
   router — components live at `/api/v1/user-components/{id}` and pushed versions
   at `/api/v1/user-components/{id}/pushed-versions/{version}`
   (`user_components.py:44,240`). This mismatch already exists at the antenna; it
   has never fired because no pushed scene carries a user component.
2. The MVP is **component-free** (canvas-chat-sponso uses none). A `FetchComponent`
   call is therefore a correct hard fetch error in the MVP.

**Decision:** R6 is a **known blind spot**, not wired for the MVP. Aligning the
antenna path (`/components` vs `/user-components`) is an authoring follow-up
(ZabCanvas issue, route through Eleven → Forge) **before** any component-bearing
scene is made selectable in local. Until then the mirror MAY omit components and
the sidecar returns 404 on R6 (a correct bundle-miss). If/when enabled, the mirror
stores under the path the **fetcher** emits (`canvas/components/<id>/<version>.json`,
§C.7), and the sidecar route→file mapping resolves the antenna mismatch locally.

### C.6 — Écarts de shape (loopback risks)

- **A — `FetchComponent` path mismatch (deferred).** Orion `/components/{id}/{v}`
  vs ZabCanvas `/user-components/{id}/pushed-versions/{v}`. Pre-existing, unexercised
  (component-free MVP). Known blind spot; R6 optional; antenna alignment is a
  separate ZabCanvas follow-up. See §C.5.
- **B — Blue graph ports + exec/data `kind` (resolved by Eleven decision B).** A
  graph `Port` (`graph.py:59`) has **no `kind`**; the `data|exec` discriminant
  `exec_partition.go::isExecNode` reads lives ONLY on `InterfacePort`
  (`graph.py:128`, served on R4) and on node-definition signatures
  (`GET /api/v1/node-definitions/{node_id}`), **not** on the compute manifest.
  Separately, Blue STORES authoring graphs whose nodes carry **empty**
  `inputs`/`outputs` (verified live `34f4b958` v5: 316 nodes, 0 ports, on both
  `/versions/{v}` and `/versions/{v}/graph`); on the antenna the editor hydrates
  node ports from `def.signature` **before publishing**. **Decision B:** the
  mirror seed contains **PUBLISHED versions that are already hydrated** (ports
  present on disk); the `httpFetcher` stays **pure pass-through** and the sidecar
  does **NOT** re-bake. `node-definitions` is a safety-net mirror path (§C.7) but
  is **not** on the hot fetch path under this decision. Verify on ≥2 real scenes
  (RC-A2/RC-A3) that published versions carry ports; if a published version is
  found portless, that is a Blue publish-time defect to fix upstream, not a sidecar
  workaround.
- **C — No ZabGate header injection on loopback.** Sidecar auth stance per §A.5
  (no-auth loopback or parity stub); 200 byte-identical either way. Bastion
  clearance before sidecar merge.
- **D — Extra fields (`status`, etc.).** Blue serves `status` on `ResolvedGraph`/
  `VersionRead` and extra fields on `BlueprintRead`/`ManifestEntryDTO` that Orion's
  structs omit; `json.Decoder` is non-strict, so verbatim mirror bytes decode fine.
  No risk.
- **E — No scalar re-serialisation.** Unlike `_query` (SQLite↔Postgres coercion,
  §A.6), canvas/blue artefacts are JSON blobs the mirror stores and serves
  **verbatim**. No coercion risk on these routes — the scalar-parity work of §A.6
  stays scoped to `_query` only.

### C.7 — Mirror on-disk layout (export ↔ sidecar interface)

> The interface between the artefact **exports** (ZabCanvas #144, Blue #174 — they
> WRITE this tree) and the **gateway sidecar** (Prism #163 — it READS this tree).
> Conduit fixes the canonical form. **Invariant: each published response body is
> stored VERBATIM at a path that maps 1:1 to the route the `httpFetcher` emits**, so
> the sidecar is a trivial **route→file** read serving bytes unchanged. No transform
> on either side; if a byte differs from the live 200 body, the hot path is broken.

Mirror root = `<sidecar-data-dir>` (Prism `userData`, loopback-only). Tree:

```
<mirror-root>/
├── canvas/
│   └── layouts/
│       └── <version>.json                          # R1  body of GET /canvas/api/v1/layouts/<version>
│                                                    #     <version> = bare sha256 64-hex (^[0-9a-f]{64}$)
│   └── components/                                  # R6 — OPTIONAL/deferred (§C.5), omitted in MVP
│       └── <id>/<version>.json                      #     body of GET /canvas/api/v1/components/<id>/<version>
├── blue/
│   ├── blueprints/
│   │   ├── <id>.json                                # R2  body of GET /blue/api/v1/blueprints/<id>
│   │   │                                            #     MUST carry current_version:int
│   │   └── <id>/
│   │       └── versions/
│   │           ├── <v>.json                         # R3  body of GET …/blueprints/<id>/versions/<v>
│   │           │                                    #     graph.nodes[*] ports PRESENT (decision B)
│   │           └── <v>/
│   │               └── graph.json                   # R4  body of GET …/versions/<v>/graph (ResolvedGraph)
│   ├── _compute-manifest.json                       # R5  body of GET /blue/api/v1/_compute-manifest
│   └── node-definitions/                            # SAFETY-NET (decision B: not hot-path); present so a
│       └── <node_id>.json                           #     future re-bake/diagnostic can read def signatures.
│                                                    #     <node_id> = namespace.name@version (filename-encoded, see below)
```

**Canonical rules (binding for #144 / #174 producers and #163 consumer):**

1. **Route→file is mechanical.** Sidecar maps the request path to a file by
   stripping the configured base, dropping `/api/v1`, and appending `.json`:
   `GET /blue/api/v1/blueprints/<id>/versions/<v>/graph`
   → `<root>/blue/blueprints/<id>/versions/<v>/graph.json`. The collection-vs-item
   collision on `blueprints/<id>` (R2 is `<id>.json`, R3/R4 nest under `<id>/`) is
   resolved by the `.json` suffix on the item and the bare dir for the subtree —
   both coexist (`<id>.json` file alongside `<id>/` dir).
2. **Verbatim bytes.** Each `*.json` = the **exact 200 response body** the live
   service emitted (`Content-Type: application/json`). No re-indent, no key
   reorder, no field drop. The export captures the live body; the sidecar serves
   it with `200 + application/json` unchanged.
3. **Version tokens.** `<version>` (canvas) = bare 64-hex, no `sha256:` prefix.
   `<v>` (blue) = the integer version, decimal, no padding (e.g. `5`, not `005`).
4. **`<node_id>` filename encoding.** `namespace.name@version` contains `@` and
   `.`; store as-is (`core.operator.on-call@1.json`) — `@`/`.` are filesystem-safe
   on all target OSes. If a node_id ever contains `/` it MUST be percent-encoded
   (`%2F`); none do today.
5. **R4 typed-miss = 404 on disk.** An absent `graph.json` for a referenced
   `(id, v)` ⇒ sidecar returns `404 {"code":"BLUEPRINT_VERSION_NOT_FOUND", …}`
   (§C.3), never an empty 200. Fail-closed is a route→file miss returning the typed
   body, not a silent skip.
6. **Mirror scope.** Seed (#A1-mirror-seed) materialises ≥2 published scenes
   (RC-A3); refresh (#A1-mirror-sync, Bastion-cleared) adds more within the
   operator's permitted perimeter. The sync mechanism is out of scope of THIS
   contract — it only changes WHICH artefacts populate the tree, never the tree
   shape or the verbatim-bytes rule.

### C.7bis — Validated-record seed (`canvas/validated/...`, Conduit A1, 2026-06-22)

> The air-eligibility seed (#247 / #144-145) was added after C.7 was written;
> this clause fixes its canonical form. It is the interface between the
> ZabCanvas export (PRODUCER — `services/mirror_export_service.py`) and Orion's
> `store.MirrorValidator` (CONSUMER — `internal/store/mirror_validation.go`).

```
<mirror-root>/canvas/validated/<scene_id>/<bare-canvas_version>.json
```

1. **Casing — snake_case.** The record is JSON `{scene_id, scene_version,
   harness_version, status, report}` in **snake_case** (the producer's
   convention). Orion's `store.SceneValidation` carries matching `json` tags so
   the unmarshal resolves; a PascalCase file would silently decode to a
   zero-value record (the original A1 bug) and the gate would refuse every
   scene. **No PascalCase.**

2. **Key — the `canvas_version`, NOT the compiled `scene_version`.** Both the
   filename (`<bare-canvas_version>.json`) and the record's `scene_version`
   field (prefixed `sha256:<canvas_version>`) are keyed by the **canvas_version**
   — the layout content address the producer pushes (`PushEnvelope.canvas_version`)
   and Orion fetches at `GET /canvas/api/v1/layouts/<canvas_version>`. The
   producer is OUTSIDE Orion and **cannot compute Orion's compiled
   scene_version** (that needs a live compile), so the canvas_version is the
   only address both sides share at push time. `MirrorValidator` resolves the
   canvas_version from the scene's latest pushed definition
   (`SceneDefinition.canvas_version`) and looks the seed up by it; the
   `sceneVersion` arg the gate seam passes (Orion's compiled scene_version) is
   ignored in embedded-local.

3. **`status` only.** The gate reads `status == "validated"`; `report` is `{}`
   in the seed (no remote campaign report reproduced). `harness_version` must
   equal Orion's `runtime.HarnessVersion` (default `"1"`).

4. **Antenne parity (RC-A1) is preserved.** The antenne path is UNCHANGED: the
   PG `scene_validations` row is minted and read keyed by the compiled
   `scene_version` (`storeAirValidator` → DB). Only the embedded-local SOURCE
   diverges (mirror file keyed by canvas_version); the air-eligibility DECISION
   is identical (a (scene, version) is eligible iff it carries a `validated`
   record at the harness). **Semantics note (RC-A5):** the *version identity*
   used to key the seed differs from antenne (canvas_version vs compiled
   scene_version) — see RC-A5 precision.

### C.8 — Verdict

**Aligned.** Contract C is derived from the live producer **and** consumer on
`origin/main`, not from memory. The five hot-path routes (R1–R5; R6 deferred) and
their shapes are unambiguous; the R4 typed-error envelope is reproduced fail-closed;
the mirror tree is a mechanical route→file map serving verbatim bytes.

- **#163 (gateway sidecar)** — UNBLOCKED. Serve R1–R5 by route→file (§C.7),
  verbatim bytes, R4 typed misses fail-closed. R6 omitted (MVP component-free).
  Bastion touch-point on loopback auth stance (écart C) before merge.
- **#144 (ZabCanvas export) / #174 (Blue export)** — UNBLOCKED. Write the §C.7
  tree: capture each published 200 body verbatim at its route→file path. Blue
  export MUST emit **published, port-hydrated** version bodies (decision B) so R3
  graphs carry node ports; node-definitions written as a non-hot-path safety net.
- **Known blind spot (écart A / decision A):** R6 component path mismatch
  (`/components` vs `/user-components`) is unfixed; aligning the antenna is a
  ZabCanvas follow-up before any component-bearing scene is selectable in local.

No breaking change to any live antenna contract: additive, a loopback transport
behind the existing `httpFetcher` shapes. Eleven decides the merge per the merge
gate.
