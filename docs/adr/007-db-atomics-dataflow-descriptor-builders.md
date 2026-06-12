# ADR 007 — core.db atomics as pure dataflow descriptor builders

- **Status**: proposed
- **Date**: 2026-06-12
- **Decided**: —
- **Deciders**: @ClodoCapeo
- **Author**: Atlas
- **Supersedes**: the `inline_graph` / "Plan C" composition model for
  `core.db.*` (referenced as "ADR 001 (QueryMe) §Plan C" in
  `Blue/src/blue/services/stdlib_seeder.py` and
  `agents/_shared/architecture.md`; **no ADR file exists in any repo** —
  the model lives only in seeds, comments and conformance reasons, and was
  never implemented in any runtime)
- **Superseded by**: —

## 1. Context

The `core.db` batch of the 81-primitive live-validation campaign exposed a
hard engine gap. Of the 7 seeded `core.db.*` nodes, only `core.db.query@1`
has an Orion executor (`execDBQuery`,
`internal/runtime/exec_effects.go` — effect node, config `datasource`,
data input `descriptor` = a complete QueryMe `QueryDescriptor`, outputs
`rows`/`count`/`elapsed_ms`/`error`). The six clause atomics
`core.db.{from,where,join,select,order,limit}@1`:

- are seeded in Blue with **already-dataflow signatures** (`plan: json` in →
  `plan: json` out, clause parameters in config);
- are classified `inline_only` in Orion's conformance manifest and
  **rejected** by the compiler in the main graph (`DB_NODE_OUTSIDE_QUERY`,
  `internal/compiler/compile.go` §(b)) with a message claiming they are
  "served transitively via `config.inline_graph`";
- but **no code anywhere reads `inline_graph`** — not Orion (`execDBQuery`
  consumes a pre-built `descriptor`), not Blue (whose own executor registers
  a handler that *raises* `db_node_outside_query`,
  `Blue/src/blue/services/executor.py`). Plan C is a dead letter.

Owner doctrine (hard constraint, see `orion-must-serve-all-of-blue`):
**Orion serves 100% of Blue**. Six seeded Blue nodes currently have no
execution path at all. The owner has ruled: implement execution — no
re-scope, no proxy.

Facts established (state of the world, 2026-06-12):

- **QueryMe v0.2.2 `compile_query` is fully implemented** — descriptor →
  SQLAlchemy `Select` with select / AND-where (closed operator set) /
  INNER joins incl. 3-way chains / order / limit / offset, schema-validated
  against the service's `SchemaDescriptor` catalog
  (`QueryMe/src/queryme/compiler.py`). The "v0.1.0 NotImplementedError
  stub" claim in the campaign notes is **stale**.
- ZabTruth and ZabRanking pin `queryme@v0.2.2` and expose
  `GET /_schema` + `POST /_query` (scope-gated `query.read.*`), which
  validate then `compile_query` + execute against **their own** DB
  (topology A, ADR 003 §3.1.3 as amended).
- The finale prod board (`bp-live-data-named`,
  `internal/runtime/named_leaderboard_test.go`) already builds the
  descriptor **in the main graph as pure dataflow**
  (`core.literal` + `core.data.set-field` + `core.data.list-append`) and
  feeds it to `db.query`'s `descriptor` input. The dataflow descriptor
  path is flight-proven; the atomics merely lack their executors.
- Per `agents/_shared/architecture.md`: one PostgreSQL per service, **no
  cross-service FK**, cross-service entity resolution via ZabGate HTTP
  only; `_query` descriptors execute against the owning service's DB,
  never a neighbour's. `ZabRanking.player_scores.player_id` mirrors
  `ZabTruth.players.id` without FK.
- Blue's seeded `core.db.query@1` contract is **desynced from the flying
  executor**: seed says config `database` + required `config.inline_graph`,
  no `descriptor` input, no exec pins; Orion's real contract is config
  `datasource`, data input `descriptor`, exec `exec_in`/`then`/`error`.

## 2. Decision drivers

1. **Doctrine**: every seeded Blue node must be executed by Orion —
   independently executable and observable at the antenna (each node's
   effect provable via the log harness). A node lowered away at compile
   time or only "transitively served" fails this bar.
2. **Minimal engine surface**: no second interpreter, no new graph
   evaluation mode, if the main dataflow engine already expresses the
   semantics.
3. **Coherence with the proven path**: the finale board proved
   descriptor-as-dataflow; the validation boundary (QueryMe validator +
   compiler on the owning service) must stay exactly where it is.
4. **Security invariants of topology A are non-negotiable**: Orion holds
   no DB credential; read-only by construction; parameterized SQL by the
   QueryMe compiler on the owning service; scopes `query.read.<service>`.
5. **Effort/testability**: pure functions over JSON are unit-testable in
   Go in isolation and provable live as leaf values.

## 3. Decision

### 3.1 Execution model — option (b): pure descriptor-builder executors

Each of the six atomics gets a **pure compute executor in Orion's existing
pure-compute registry** (`internal/runtime/compute_pure.go` tranche
pattern), composable in the **main graph** as ordinary dataflow. The
`plan` value flowing between them **IS the QueryMe `QueryDescriptor` JSON**
— no intermediate format, no conversion step: the chain's final `plan`
wires directly into `core.db.query@1`'s `descriptor` input.

Node semantics (over the canonical shape
`{table, where:[], joins:[], select:[], order:[], limit?, offset?}`):

| Node | Inputs | Effect on plan |
|---|---|---|
| `from@1` | — (config `table`) | emits `{table, where:[], joins:[], select:[], order:[]}` |
| `where@1` | `plan`, `value?` (config `column`, `op`) | appends `{column, op, value}` to `where` (`IS NULL` → value normalised to null) |
| `join@1` | `plan` (config `table`, `local_column`, `foreign_column`, `select`) | appends `{table, on:[local,foreign], select}` to `joins` |
| `select@1` | `plan` (config `columns`) | sets `select` (FROM-table projection; joined columns project via the join's own `select`) |
| `order@1` | `plan` (config `column`, `direction`) | appends `{column, direction}` to `order` (stacking = tie-break order) |
| `limit@1` | `plan`, `n?` (config `n` default 100) | sets `limit` (input `n` overrides config) |

**Totality rule**: the atomics are *total* pure functions — they never
error. A malformed upstream `plan` (non-object) is replaced by the empty
base shape before applying the clause. **All semantic validation stays
server-side** in the QueryMe validator/compiler on the owning service
(unknown table/column, operator/value shape, type issues), surfacing on
`db.query`'s `error` output exactly as today (`DB_QUERY_FAILED: …` carrying
the structured 400 issues). One validation boundary, not two — Orion does
not re-implement the whitelist.

**Rejected alternatives**:

- *(a) Implement Plan C `inline_graph`*: requires a second, nested graph
  evaluator inside `execDBQuery` (sub-graph walker + `binds` mechanism +
  its own compile/diagnostic path) for **zero added expressiveness** over
  the main dataflow engine — the atomics' seeded signatures are already
  plain dataflow. Doubles the engine surface, makes each atomic observable
  only *through* its parent (violating per-node provability), and
  contradicts the flight-proven finale pattern. Plan C also has no source
  document to honour — it exists only in comments.
- *(c) Compile-time lowering in Blue* (collapse an atomic chain into a
  descriptor literal before Orion sees it): the nodes would never be
  *executed* by the engine, only erased — fails the doctrine's
  observability bar and creates a second compiler path to keep in sync.

### 3.2 Blue resync (seed + preview executor)

- `core.db.query@1` seed is **amended to the real Orion contract**:
  config `datasource` (gateway prefix, replaces `database`); data input
  `descriptor` (json, required); optional `timeout_seconds`; exec pins
  `exec_in` / `then` / `error`; outputs `rows`/`count`/`elapsed_ms`/`error`.
  `config.inline_graph` is **removed** (it was required-but-dead; no
  flying board uses it — the finale board predates no one: it feeds
  `descriptor`).
- The six atomics' seeded descriptions drop all `inline_graph` / "parent
  query" language; `join`'s description keeps the **same-datasource**
  constraint (§3.4).
- Blue's preview executor replaces the `db_node_outside_query`-raising
  handlers with **real implementations of the same six pure semantics**
  (parity with §3.1, shared descriptor fixtures), so editor test-node and
  Orion agree. The `db_node_outside_query` error code dies in both
  codebases.

### 3.3 Conformance manifest & compiler

- `KindInlineOnly` is **retired**. `scripts/gen_conformance_manifest.py`
  drops the `inline_only` flag; the six atomics become ordinary pure
  computes with executors; manifest regenerated and vendored.
- Compiler: the §(b) structural guard (`ErrDBNodeOutsideQuery`) is
  **removed**; the atomics flow through the normal manifest lookup and
  the pure registry. The diagnostic code is deleted (no deprecation
  period — nothing can have authored it successfully).
- CI `conformance-matrix` must assert **7/7 `core.db.*` nodes served with
  a real executor** (six pure + one effect), and assert zero `inline_only`
  entries remain.

### 3.4 `join` semantics — single-datasource only (normative)

`join` is **restricted to tables of the same service catalog** (same
datasource, same DB). This is not a capability refusal — it is structural
truth: a descriptor executes against exactly one service's DB (topology A),
that service's `SchemaDescriptor` contains only its own tables, and there
are no cross-service FKs. A descriptor joining a foreign table fails the
owning service's validator and surfaces on `db.query.error` — the same
*declaration-missing* error class as `DATASOURCE_NOT_DECLARED`, fully
served, observably failing.

**Cross-service joins are explicitly out of scope as a SQL concept.**
Cross-service composition is an *application-level pattern the language
already serves*: two `db.query` nodes (one per datasource) composed with
`core.data.*` nodes, or `http.request` via ZabGate — exactly how the
platform resolves `player_id → summoner_name` today. No gateway-side
"applicative join" engine is built: it would create a hidden N+1 HTTP
amplifier and a second query planner for a need the finale board already
met with explicit composition. If a recurring authored pattern emerges,
that is a future *Blue component* (authored, reusable — per the
"transitions are authored scenes" doctrine analogue), not an engine
feature.

**Consequence for the harness**: the `db.log5` join line re-targets two
same-service tables — `player_scores JOIN splits ON split_id=id`
(datasource `ranking`, project `splits.name`) — making `join` provable
live. The cross-service pair (`player_scores` × `players`) moves to a
**negative line**: it must render the validator error on `error`, proving
the boundary honestly.

### 3.5 QueryMe — no change required

`compile_query` v0.2.2 already implements everything §3.1 produces
(incl. multi-join chains, order stacking, limit/offset); services pin
v0.2.2 and their `_query` endpoints are live. **No QueryMe issue is
opened.** If implementation uncovers a real QueryMe gap, that is a scope
stop → back to Atlas, not an inline fix.

## 4. Consequences

- The six atomics become first-class, individually executable, individually
  observable dataflow nodes; the campaign's db batch can re-author its
  board on the *actual* atomics instead of `set-field`/`list-append`
  scaffolding.
- One descriptor format end-to-end (atomics → `db.query` → `_query` →
  QueryMe), one validation boundary (owning service).
- Blue seed + Orion executor contracts are resynced (today's seed for
  `query@1` is unauthorable as-is).
- Dead concepts removed: `inline_graph`, `db_node_outside_query`,
  `KindInlineOnly` — less surface, less lying documentation.
- ADR 003/006 conformance language about "served transitively" becomes
  obsolete where it concerns `core.db.*`; this ADR is the new reference.

## 5. Risks

1. **Prism editor coupling** (low): if Prism's blueprint editor
   special-cases `inline_graph` or the `db_node_outside_query` code,
   it needs a follow-up resync. To verify during B1; no Prism issue
   opened pre-emptively.
2. **Seed migration on deployed Blue** (medium): amending `query@1`'s
   signature requires the stdlib re-seed path to update an existing node
   definition in place; any persisted board authored against the stale
   seed (none known to fly besides `bp-live-data-named`, which matches
   the *new* contract) must be checked.
3. **Security — no new attack surface** (assessment): a fully
   author-controlled descriptor reaching `_query` was **already possible**
   (the finale board composes one from `set-field`/`list-append`); the
   atomics add a nicer authoring syntax for the same payload. The
   validation boundary (QueryMe whitelist + scope gate `query.read.*` +
   read-only-by-construction + parameterized SQL on the owning service)
   is untouched; Orion still holds no DB credential. **Bastion clearance
   not required by this ADR**; if Vigil or the owner disagrees, the gated
   spawn is Eleven's call.
4. **Totality choice** (accepted): atomics silently normalising a
   malformed plan (instead of erroring) could mask an authoring mistake
   until `db.query` errors. Accepted: the error still surfaces, on the
   single boundary, with structured issues; pure nodes stay total like
   the rest of the pure tranche.

## 6. Resolution criteria (testables)

1. **Orion**: each of the six atomics has a pure executor; Go unit tests
   cover each clause op (golden plan-in → plan-out), chain composition
   `from→where→join→select→order→limit` produces a descriptor that QueryMe
   v0.2.2 accepts byte-for-byte (fixture shared with Blue), and a
   compiled board placing all six in the **main graph** compiles with
   zero diagnostics.
2. **Conformance**: regenerated manifest contains zero `inline_only`
   entries; `conformance-matrix` CI job asserts 7/7 `core.db.*` served
   with executors; `DB_NODE_OUTSIDE_QUERY` no longer exists in the Orion
   codebase.
3. **Blue**: seeded `core.db.query@1` matches the Orion executor contract
   (datasource / descriptor / exec pins / outputs); the six preview
   handlers execute with semantics identical to Orion against the shared
   fixtures; `db_node_outside_query` removed; Blue + ZabCanvas test
   suites green.
4. **Antenna proof (campaign gate)**: the db harness batch re-authored on
   the real atomics goes live via the standard activation script; the 7
   lines prove from / select / where / order / limit / join / query-error
   per the runbook's per-line mapping, with `db.log5` =
   `player_scores ⋈ splits` (datasource `ranking`) showing a joined
   column live, plus a negative line proving the cross-service join
   boundary on `error`. Log evidence captured in the runbook.
5. **No QueryMe release** is cut for this ADR; services stay on v0.2.2.
