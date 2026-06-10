# ADR 003 — Dataflow-only runtime (exec-pin rejection) + platform-event ingestion via Quasar push

- **Status**: proposed
- **Date**: 2026-06-10
- **Decided**: —
- **Deciders**: @ClodoCapeo
- **Author**: Atlas (architect agent)
- **Supersedes**: —
- **Superseded by**: —

---

> **Why this ADR lives in `Orion/docs/adr/` and is numbered 003.** Both decisions
> are owned by Orion: the runtime execution model (`internal/runtime/scene.go`,
> `compute.go`) and the compiler's acceptance contract
> (`internal/compiler/compile.go`). 001/002 established the per-repo convention;
> 003 is the next free number. Blue and Quasar carry **consequences**
> (cross-referenced, §4.3), not independent architectural choices.

## 1. Context

The engine audit (Lens, 2026-06-10, `build/audit-engines/logic-gaps.md`) found two
blocking breaks in the Blue → Orion → Solar chain. All claims below were
re-verified in code by this ADR's author.

### 1.1 Break 1 — accept-then-ignore of the exec sub-language

Blue describes a ~70-node Unreal-Blueprint-style language including an
**imperative exec-pin sub-language**: `core.flow.branch / sequence / gate /
for-loop / for-each / while / delay`, `core.variable.set`, `core.print`
(`Blue/src/blue/services/stdlib_seeder.py:131-262,819-850`). Orion v2 is a
**pure dirty-driven dataflow runtime**: `recompute` (`scene.go:315-365`) is a
topo-sorted walk that re-evaluates pure functions when an upstream leaf is
dirty. It has **no notion of exec pins, sequencing, loops, timers or branches**.

The gap is silent on both gates:

- Blue's purity map stamps branch/sequence/gate/loops as `pure+bounded`
  (`node_purity.py:41-47`) — which is true *as functions*, so they pass Orion's
  compile gate (`validateBlueprint`, `compile.go:492-574`, which checks only
  manifest presence + `is_pure`). The compiler **never checks the runtime
  registry**.
- The runtime registry holds **13 entries** (`compute.go:41-78`: 5 math,
  6 compare, `not`, `select`, plus the `core.output@1` passthrough). At the
  antenna, an unregistered compute hits `cmpReg.Get` → error → `recompute`
  **logs and `continue`s** (`scene.go:341-345`). The node's leaf is never
  written. A blueprint with a `branch` is accepted at push, then silently does
  nothing on air.

The same silent path also swallows the ~30 **pure data nodes** Blue declares
but Orion hasn't registered yet (logic `and/or/xor`, extended math, string,
cast, `data.get-field`/`list-*`) — those are registry gaps, not a model
mismatch, but they ride the same accept-then-ignore hole.

Adjacent defect that caps any registry growth: edge `to_port` names are **not
carried into the graph artefact**; `gatherInputs` wires upstreams positionally
onto `a,b,c,d` modulo 4 (`scene.go:335-339,367-377`), with per-compute name
fallbacks as a workaround (`compute.go:121,170`).

### 1.2 Break 2 — platform events have no producer wired end-to-end

Blue ships 14 `quasar.twitch.*@1` leaf-input nodes whose
`signature.platform.leaf_path` declares the ADR 005 §9 convention
`__inputs.platform.<platform>.<channel>.<event_alias>`
(`Blue/src/blue/nodes/quasar/_shared.py:43-45`, `twitch.py`).

What **already exists** (the audit under-credits this):

- **Quasar has a complete push client**: `quasar/core/orion_client.py` holds a
  long-lived WS to Orion `/show/stream` with a ZabAuth **service token** scoped
  to `__inputs.platform.*` paths, exponential-backoff reconnect, token refresh,
  and a bounded buffer (default 1 000, oldest-drop). It sends each canonical
  event as an ADR 002 `input` message at `leaf_path(event)`.
- **Orion already authorizes scoped service writes**: the show WS handler
  forwards inputs to `Inbox.Write` which enforces the identity's path scope
  (`ws/server.go:132-148`, `auth/identity.go:98` pattern matching
  `__inputs.platform.*`, tested in `auth/identity_test.go`).

What is actually **broken**:

1. **The compiler never binds `quasar.*` nodes to their platform leaf.**
   `nodeLeafPath` (`compile.go:584-599`) only knows `core.output/input/literal`;
   a `quasar.twitch.chat@1` node compiles to `Path == ""`, `Kind == "input"`
   (no upstream), so its state address falls back to its **node id**
   (`scene.go:359-362,379-392`). Quasar writes
   `__inputs.platform.twitch.<channel>.last_chat`; the graph reads
   `<node-uuid>`. The two never meet. The manifest already carries the
   `platform` block (`compiler/types.go:310`, fixture
   `testdata/blue_compute_manifest.json`) — it is simply unused.
2. **The leaf-path conventions disagree.** Blue's default `event_alias` is the
   raw event type (`_shared.py:94` → `…<channel>.chat`); Quasar's normalizer
   emits `last_<type>` (`normalizer.py:27` → `…<channel>.last_chat`); Orion's
   protocol fixture also uses `last_chat` (`protocol/messages_test.go:90`).
   2-of-3 repos say `last_<type>`; Blue is the odd one out. No contract test
   pins this, so it drifted unnoticed.
3. **Nobody expands `<channel>`.** The placeholder must be resolved "at scene
   compile time using the operator's connected channel handle" (`_shared.py`),
   but the envelope, node config and compiler have no channel anywhere.
4. **`extractAdapters` is a stub** returning `nil` (`compile.go:692-698`).
   This matters for **http-poll / pg-listen** (their runtime adapters exist but
   the compile path never declares bindings) — but per the decision below it
   does **not** gate platform events, which need no Orion-side adapter at all.

### 1.3 Transverse constraint (non-negotiable, set by the maintainer)

**No more accept-then-ignore.** Every blueprint Orion accepts at push must be
served in full at the antenna; anything it cannot serve must be rejected at
validation with a clear, actionable error.

## 2. Decision drivers

- **Orion v2's value is its guarantees**: bounded, terminating, dirty-driven
  recompute over pure functions; restart reseeds from defaults (criterion 11);
  ≤ 50 ms input-to-delta. Any execution model added must not erode these.
- **Fail-closed over fail-silent** (§1.3). A loud compile error is an authoring
  feature; a silent no-op on air is a production incident.
- **Single source of truth, no drift by construction.** The compiler and the
  runtime registry live in the **same binary** — the acceptance gate can check
  the real registry instead of a second list that would drift.
- **Don't rebuild what exists.** Quasar's push client, ZabAuth service tokens,
  Orion's scoped-write enforcement and the `__inputs.platform.*` namespace are
  built and tested. The ingestion decision should wire the last mile, not
  introduce a new transport.
- **Broadcast-overlay semantics.** Orion state is *latest value per leaf*, not
  an event log. Restart deliberately loses live state. The event semantics
  chosen must be honest about that, not promise stream-processing guarantees
  the model can't keep.
- **Blue stays the language authority; Orion stays the antenna authority.**
  Blue's validator returns findings and must keep accepting drafts freely;
  what is *runnable on air* is Orion's compile contract.

## 3. Decision A — Orion stays dataflow-only; the exec sub-language is rejected at compile, loudly

**Verdict: no exec engine in Orion.** Orion v2 remains a pure dirty-driven
dataflow runtime. Blueprints using exec-pin control-flow nodes are **rejected
at push** with a per-node diagnostic. The growth path for expressiveness is
**pure data-node registry expansion** (and, later, per-node lowering of
specific exec idioms to pure dataflow equivalents) — never an imperative
interpreter inside the reactive loop.

### 3.1 The gate: compiler validates against the live runtime registry

`validateBlueprint` gains a third check after manifest-presence and purity:
**the compute id must be executable by the runtime**. The compiler receives
the registry's key set (`ComputeRegistry` exposes `Has(id)` / `IDs()`;
`Compile` already runs in the same process — `cmd/orion/main.go` wires both).
A node whose compute is pure-in-manifest but absent from the registry yields a
new diagnostic:

```
UNSUPPORTED_COMPUTE  blueprint node <id> uses compute "core.flow.branch@1",
                     which this runtime cannot execute (dataflow-only, ADR 003).
                     Data-flow conditionals: use core.flow.select@1.
```

Push fails with `COMPILE_FAILED` carrying these diagnostics, exactly like
`UNKNOWN_COMPUTE`/`IMPURE_COMPUTE` today. Nothing is persisted. The runtime's
log-and-continue in `recompute` (`scene.go:341-345`) stays as defense-in-depth
against drift, but gains a counter metric (`orion_compute_unknown_total`) that
must remain **0** for any compiled scene — observable proof that
accept-then-ignore is dead.

Permanently unsupported under this model (rejected, with the message naming
the alternative where one exists): `core.flow.branch` (→ `select`),
`sequence`, `gate`, `for-loop`, `for-each`, `while` (→ future pure
`core.data.map`/`fold`/`aggregate` lowering, separate ADR when a real scene
needs it), `core.variable.get/set`, `core.print`. (`delay`, `http-request`,
`db.query`, `source.read`, `animation.play` are already rejected as impure —
unchanged.)

### 3.2 Prerequisite: edges carry port names into the graph artefact

"Served in full" requires correct wiring, not positional luck. The compiler
carries each edge's `to_port` into the graph artefact (`GraphNode.Upstream`
becomes `[]{NodeID, Port}` or a parallel map); `gatherInputs` delivers values
under their **declared port names**, keeping the positional `a,b,c,d` synthesis
only as a fallback for artefacts lacking port names (stored scenes recompile on
next push; no migration). The per-compute name-fallback chains in `compute.go`
become dead-code-by-default rather than load-bearing.

### 3.3 Growth: register the pure data nodes Blue already declares

With the gate in place, registry coverage becomes an honest, incremental
`/build` axis. First tranche (all `_PURE_BOUNDED` in `node_purity.py`, all
needed by realistic overlays): `core.logic.and/or/xor`,
`core.math.abs/min/max/clamp/lerp/round/floor/ceil`,
`core.string.concat/format/length/split/upper/lower`, `core.cast.to-*`,
`core.data.get-field/set-field/list-length/list-at/list-append/aggregate`.
`core.data.get-field` is **required by Decision B** (extracting fields from
canonical event payloads). Each entry ships with unit tests over named ports
(§3.2). `core.db.*` plan-builder atomics stay out (worthless without the
impure `db.query` executor — future ADR if reactive DB reads become a need).

### 3.4 Alternatives evaluated and rejected

- **(A) Exec engine inside Orion** (a second interpreter walking exec pins on
  trigger). Rejected: it forks the runtime into two execution models with
  undefined interleaving against the dirty-driven loop; `while`/`delay` break
  the termination/boundedness guarantees that criterion 18 and the purity gate
  exist to protect; state mutation mid-recompute invalidates the topo-order
  contract; and no current scene needs it — the priced-in cost is permanent,
  the benefit speculative.
- **(B) Compile all exec pins to dataflow.** Partially sound (`branch` data-
  lowers to `select`) but unfaithful in general: `sequence`/`gate` have no pure
  equivalent (their meaning *is* ordering/suppression of effects), loops over
  time have no dataflow translation, and a "best-effort" lowering that changes
  semantics silently is accept-then-ignore wearing a costume. Retained only as
  a **future, per-node, explicitly-specified** lowering path (e.g. `for-each`
  → pure `map` over a list leaf) in a follow-up ADR.
- **(C) Status quo (silent ignore).** Inadmissible per §1.3.

## 4. Decision B — Platform events are pushed by Quasar over the existing service-token WS; Orion's compiler binds the declared leaves

**Verdict: no Orion-side `platform-stream` adapter, no new service, no new
transport.** Quasar (which owns the Twitch connections, OAuth and
normalization) remains the single producer and pushes canonical events to
Orion's show WS as scoped `input` writes — the path ADR 005 §9 designed and
that `orion_client.py` already implements. Orion's job is purely
compile-side: bind `quasar.*` nodes to their `__inputs.platform.*` leaves so
the pushed values actually feed the graph.

Why push beats the alternatives: a compiler-declared Orion-side adapter
(à la http-poll) would require Orion to hold platform credentials or a
back-channel to Quasar per scene, duplicate connection lifecycle Quasar
already owns, and tie event flow to scene compilation; a separate bridge
service adds a hop, a deploy and an auth surface for zero capability. Push
through the gateway with a path-scoped service token is the smallest surface
and is already built and Bastion-reviewed (C3) on the Quasar side.

### 4.1 One canonical leaf convention

`__inputs.platform.<platform>.<channel>.last_<event_type>` — channel
casefolded, charset `[a-z0-9_]+`. This is what Quasar's normalizer emits and
Orion's protocol fixtures use; **Blue aligns** (its default `event_alias`
becomes `last_<event_type>` in `_shared.py`), keeping `signature.platform.
leaf_path` the authoring-time source of truth the compiler reads. The `last_`
prefix is semantic, not cosmetic: the leaf holds the **latest** event, per the
state model (§4.4).

### 4.2 Compiler binding + channel expansion

For any manifest entry carrying a `platform` block (`types.go:310`),
`validateBlueprint`:

1. reads `signature.platform.leaf_path`;
2. expands `<channel>` from the **node's authored config**
   (`n.Config["channel"]`, set by the author in Prism when picking the
   connected account) — casefolded, validated against `[a-z0-9_]+`;
   missing → `PLATFORM_CHANNEL_MISSING`, invalid → `PLATFORM_CHANNEL_INVALID`
   (fail-closed; an unvalidated channel string would otherwise inject dots
   into the leaf namespace);
3. emits the node as `Kind: "input"`, `Path: <expanded leaf>` — from there the
   existing machinery just works: Quasar's `Inbox.Write` lands on that leaf,
   the dirty walk recomputes downstream, deltas fan out.

The node's value at the leaf is the **whole canonical event** (payload, actor,
ts, channel — as Quasar sends it); downstream extraction uses
`core.data.get-field` (§3.3 dependency). Exposing the four declared output
ports as separate sub-leaves is deferred until named-port wiring (§3.2) has
landed and a real scene needs it.

Node config (channel) participates in the compiled content, hence in
`scene_version` — correct: rebinding a scene to another channel is a new
version. Per-key namespacing from ADR 001 §3.3 does **not** apply to
`__inputs.*` leaves (they are a shared external namespace, deliberately
addressable by any blueprint in the scene).

Scoped out, unchanged: `extractAdapters` remains a stub — its prod wiring for
**http-poll / pg-listen** bindings is a real adjacent gap (audit §2) owned by
the Canvas-extensions chantier, tracked as a follow-up, **not** required for
platform events (no Orion-side adapter exists in this design, so there is
nothing for it to declare).

### 4.3 Cross-repo consequences (Blue, Quasar)

- **Blue**: `_shared.py` default alias → `last_<event_type>`; stdlib seeder
  re-stamps the 14 `quasar.twitch.*` definitions (idempotent update of
  `signature.platform.leaf_path`; same backfill discipline as the purity
  migration `b1u3a0000004`).
- **Quasar ↔ Blue contract test**: a shared fixture asserts, for every entry
  of `CANONICAL_EVENT_TYPES`, that Quasar's `leaf_path(event)` byte-equals
  Blue's declared `signature.platform.leaf_path` with `<channel>` expanded —
  the same vendored-mirror discipline as `test_canonical_compat.py`. This is
  the test whose absence let §1.2-2 drift.

### 4.4 Event semantics (decided, documented, honest)

- **Latest-value, last-write-wins.** A platform leaf holds the most recent
  canonical event of its type for its channel. Orion state is not an event
  log; bursts coalesce in `drainNonBlocking` by design.
- **Ordering**: FIFO per WS connection (TCP). No cross-event-type ordering
  guarantee.
- **Delivery: at-most-once across disconnects.** Quasar buffers up to
  `QUASAR_ORION_BUFFER_SIZE` (1 000) during reconnect windows and drops oldest
  beyond — losing late history is preferable to unbounded memory (existing,
  affirmed). No replay, no persistence; Orion restart reseeds defaults
  (criterion 11 intact).
- **No backpressure propagation to Twitch**: Quasar reads ack frames to avoid
  TCP backpressure; Orion's inbox absorbs bursts; overload degrades to
  coalescing, never to blocking the EventSub/IRC readers.
- **Out of scope, explicit**: counting/aggregating events over time (sub
  counters, hype trains) needs stateful reducer nodes — a future ADR. Authors
  get "react to the latest event" in v1, which is what overlays
  (alerts, last-sub banners) actually need.
- **Writes to undeclared leaves**: a scoped service write to a platform leaf
  no active-scene node declares is accepted into state (current `Inbox`
  behavior) and broadcast as a delta. Kept in v1 for simplicity; flagged to
  Bastion (R4) — chat payloads are user-generated content reaching all
  subscribers.

## 5. Consequences

- The accept-then-ignore class dies: what compiles, runs; what can't run,
  fails the push with a named node and a named reason. Prism surfaces compile
  diagnostics to the author (it already renders `COMPILE_FAILED` bodies).
- A Twitch-reactive overlay becomes authorable end-to-end for the first time:
  `quasar.twitch.chat@1` → `get-field` → `select` → `core.output@1`, fed live
  by the existing Quasar WS.
- Orion's runtime loop is **untouched** in its model — all changes are
  compile-side gates, registry entries and port-name plumbing. The ≤ 50 ms and
  restart-reseed guarantees are preserved by construction.
- Authoring restriction made explicit: Blue keeps describing the exec
  sub-language (drafts, future runtimes), but on-air scenes are dataflow-only.
  Prism palette filtering by runtime support (e.g. an Orion endpoint exposing
  registry ids) is a deferred UX follow-up, not part of this ADR.
- Existing scenes containing exec/unregistered nodes will **start failing at
  re-push** (not at load). Deliberate: they were silently broken on air
  already; the failure now happens where the author can see and fix it.
- The `last_` alias change re-stamps Blue's stdlib rows — synchronized
  Blue release; Quasar/Orion need no wire change (they already use
  `last_<type>`).

## 6. Risks

Security-surfaced risks → **Bastion** (do not self-clear).

- **R1 — Compile gate breaks existing authored blueprints.** Any stored
  blueprint using exec nodes fails its next push. Accepted (it never worked on
  air; failing loudly is the fix). Mitigation: the diagnostic names the node
  and the data-flow alternative.
- **R2 — Channel string is author-controlled input entering a state path.**
  Mitigated fail-closed by charset validation + casefold (§4.2). → Bastion:
  confirm the validation closes path-injection / scope-escape (a crafted
  channel must not produce a leaf outside `__inputs.platform.<platform>.*`).
- **R3 — Service-token scope breadth.** Quasar's token is scoped
  `__inputs.platform.*` (all platforms, all channels). Bounded by ZabAuth
  path-scoping and the gateway; → Bastion: confirm scope granularity is
  acceptable or should narrow to per-platform.
- **R4 — UGC fan-out.** Chat payloads (arbitrary viewer text) land in state
  and broadcast to every subscriber, including for leaves no scene declares
  (§4.4). Rendering safety is Solar/Lumencast's job, but the *transport* of
  unsolicited UGC is new surface. → Bastion: assess; if rejected, the fallback
  is compile-declared-leaf filtering in `Inbox` (cheap follow-up, an
  allowlist Orion already has at scene load).
- **R5 — At-most-once loss windows.** Reconnects can drop events (>1 000
  buffered). Accepted and documented (§4.4); overlays show latest state, they
  are not ledgers. Anything money-adjacent (sub counts) must come from a
  queryable source of truth later, not this stream.
- **R6 — Registry/manifest drift.** Eliminated by construction for the
  accept-gate (same-process registry check). Residual: Blue manifest purity
  vs Go behavior — unchanged from today, covered by criterion 18.
- **R7 — http-poll / pg-listen bindings still undeclared in prod**
  (`extractAdapters` stub). Out of scope here, **tracked** as a follow-up to
  the Canvas-extensions chantier so it is not lost (audit §Incertitudes).

## 7. Resolution criteria

Testable; CI-enforced where possible.

1. **Exec rejection.** Pushing a blueprint containing `core.flow.branch@1`
   (and one containing `core.variable.set@1`) returns `COMPILE_FAILED` with an
   `UNSUPPORTED_COMPUTE` diagnostic naming the node id and compute id; no
   scene definition row is persisted.
2. **Acceptance ⊆ registry (no accept-then-ignore).** A compiler test asserts
   every compute id accepted by `validateBlueprint` over the manifest fixture
   is present in `NewComputeRegistry()`; a runtime test asserts
   `orion_compute_unknown_total` stays 0 across a full recompute of any
   compiled scene.
3. **Named ports.** The graph artefact carries edge `to_port`; a
   `core.flow.select@1` whose edges are authored in shuffled order still
   selects correctly (test wires `when_false, condition, when_true` in that
   order).
4. **Registry tranche.** Every §3.3 compute is registered with unit tests
   (named-port inputs, null/missing-input behavior) and accepted by the
   compiler gate.
5. **Platform leaf binding.** A blueprint with `quasar.twitch.chat@1`
   (`config.channel = "ZabChannel"`) compiles to a graph node with
   `Path == "__inputs.platform.twitch.zabchannel.last_chat"`; missing channel
   → `PLATFORM_CHANNEL_MISSING`; channel `"a.b"` → `PLATFORM_CHANNEL_INVALID`.
6. **Cross-repo leaf contract.** For all 14 canonical event types, Blue's
   declared `signature.platform.leaf_path` (channel expanded) byte-equals
   Quasar's `leaf_path(event)` — asserted by a shared-fixture test present in
   both repos' CI.
7. **End-to-end platform event.** E2E: a service-token WS client (Quasar
   shape) sends a canonical chat event `input`; a scene whose graph wires
   `quasar.twitch.chat@1 → get-field(payload.text) → core.output@1` emits the
   delta on the output leaf to a subscriber in ≤ 100 ms; an out-of-scope path
   write gets `WRITE_FORBIDDEN`.
8. **Restart semantics intact.** After restart, platform leaves are reseeded
   from defaults (no persisted live state) — existing criterion 11 test
   extended to a platform leaf.
9. **Org gates.** Orion CI green (vet/test/build/staticcheck/golangci/
   trufflehog); Blue + Quasar CI green (ruff/mypy/pytest/pip-audit); review
   approved by **Vigil**; **Bastion** clearance on R2/R3/R4 before any merge
   touching the ingestion surface.
