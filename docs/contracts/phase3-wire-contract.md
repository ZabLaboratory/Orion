# Phase-3 wire contract (ADR 003 Amendment 1) — Conduit

> Conduit-finalized wire forms for Forge #85/#86. Proven against both sides
> of the code (producer + consumer + gateway), 2026-06-10. NOT self-merged:
> every diff is Vigil-reviewed in its repo; Bastion re-clearance gates the
> phase-3 merge (R2 egress + `_query` + completion). Eleven decides the merge order.

This doc fixes the exact strings/shapes. It does not implement anything.

---

## 0. The blocking cross-repo prerequisite (read first)

**`X-Authenticated-Paths` is injected by ZabGate on the WS proxy only, never on
the REST proxy.**

- WS path injects it (`ZabGate/src/zabgate/proxy/ws.py:91,338`,
  `_AUTH_PATHS_HEADER`, gated on `role=service`).
- REST path does NOT (`ZabGate/src/zabgate/proxy/http.py:121-129` injects only
  `x-authenticated-user` + `x-authenticated-role`; the JWT `paths` claim is
  decoded nowhere on REST — `header.py:248-251` sets only `state.user`/`role`/`token`).

Both phase-3 REST wires need it:
1. Orion → `POST /<svc>/api/v1/_query` (Orion's service token must carry its
   `_query` scope so the owning service can enforce it).
2. Renderer → `POST /orion/.../exec/completion` (the completion scope is enforced
   by Orion's existing `CanWritePath`, which reads `X-Authenticated-Paths`).

**⇒ ZabGate must inject `X-Authenticated-Paths` on the REST proxy for
`role=service`, mirroring the WS path** (decode `paths` claim in `header.py`,
authoritative override via `/validate` like `ws.py:203-212`, CSV-join in
`http.py`). This is a **ZabGate prerequisite issue, separate from #85**, and it
**blocks the runtime enforcement** of both wires (the gates would otherwise see
empty paths and fail-closed). The Orion-side and service-side code can be built
in parallel; the E2E smoke test cannot pass until the ZabGate REST injection lands.

---

## 1. `db.query` → topology A (`_query` delegation)

### 1.1 Endpoint (already exists — verified)

`POST /<svc>/api/v1/_query`, reached by Orion as `POST /<svc>/api/v1/_query` via
ZabGate (prefix `/truth`, `/ranking`, …). Confirmed live in
`ZabTruth/src/zabtruth/routes/internal.py` and
`ZabRanking/src/zabranking/routes/internal.py`. Companion `GET /<svc>/api/v1/_schema`.

**Request body** = QueryMe `QueryDescriptor` JSON (`QueryMe/src/queryme/descriptor.py`,
`extra="forbid"`):

```json
{
  "table": "players",
  "where": [{"column": "team", "op": "=", "value": "ZAB"}],
  "joins": [{"table": "matches", "on": ["match_id", "id"], "select": ["patch"]}],
  "select": ["summoner_name", "role"],
  "order": [{"column": "summoner_name", "direction": "asc"}],
  "limit": 50,
  "offset": 0
}
```
Operators (closed list): `= != > < >= <= IN LIKE "IS NULL"`. Read-only by
construction (no mutation descriptor exists in QueryMe).

**Response** (200):
```json
{"rows": [ {"<col>": <scalar>, ...}, ... ], "count": 3, "elapsed_ms": 12}
```
Columns = `descriptor.select ++ each join.select`, collisions qualified `<table>.<col>`.

**Error** (invalid/uncompilable descriptor): **400** with
`{"detail": {"issues": [ {<structured issue>}, ... ]}}` (from `validate_against_schema`
or `CompilationError.issues`). Note: FastAPI wraps under `detail`.

### 1.2 Gap on the service side (in scope of #85's cross-repo dependency)

Today `_query` has **no scope enforcement** — its docstring says it trusts the
JWT/user path (`X-Authenticated-User` injected by ZabGate, for the Blue editor).
For Orion's **service-token** call, the owning service MUST gate `_query` on the
service-token scope (read `X-Authenticated-Role: service` + `X-Authenticated-Paths`
containing the `_query` scope). **This is a per-service change** (ZabTruth,
ZabRanking, any DataSource-owning service) — a cross-repo dependency, see §3.

### 1.3 Orion service-token scope string (exact)

Service-token `paths` are dot-prefix patterns enforced by `CanWritePath`
(`Orion/internal/auth/identity.go:82-116`) — they are **write-scope authorities**
over Orion state leaves, NOT a generic RPC scope. The `_query` call is Orion acting
as a **client** of another service; the scope that authorizes it is checked by the
**owning service**, not by Orion's `CanWritePath`. So the string namespace is the
service's, not Orion's leaf space.

**Decision needed from Eleven/Atlas (ambiguous — flagged):** the scope granularity.
Two coherent options, pick one platform-wide:
- **(A) per-service:** `query.read.<svc>` (e.g. `query.read.truth`, `query.read.ranking`).
- **(B) per-datasource:** `query.read.<datasource>` if a service can own several.

Recommended: **(A) `query.read.<svc>`** — matches the one-service-owns-its-DB
segmentation and the `/<svc>/` routing prefix already used by ZabGate. Orion's
token then carries the union of the services it is allowed to query, e.g.
`paths = ["query.read.truth", "query.read.ranking"]`. The owning service checks
its own name is present.

> NB: this scope is NOT an Orion `__`-leaf pattern. The owning service reads
> `X-Authenticated-Paths` and asserts `query.read.<self>` ∈ paths. Keep it out of
> the `__inputs.*` / `__system.*` namespace to avoid collision with Orion's write scopes.

**Provisioning:** operator mints Orion's service token via
`POST /auth/api/v1/service-tokens` (`ZabAuth/src/zabauth/routes/service_tokens.py`),
body `{"service": "orion", "paths": ["query.read.truth", ...], "ttl_s": 3600}`.
The `paths` allow-list is operator-curated at mint time. ttl ≤ 1 h, refresh via
`/service-tokens/refresh`.

### 1.4 `ORION_DATASOURCES` format (étage-1 config, exact)

A DataSource is a logical name authored in a Blue `db.query` node. Orion's compiler
must resolve it to (gateway URL prefix + scope). Recommended form — CSV of
`name=svc` triples, scope derived as `query.read.<svc>`:

```
ORION_DATASOURCES=truth=truth,ranking=ranking
```
i.e. `<logical_name>=<zabgate_prefix_svc>`. Resolution:
- gateway URL = `${ZABGATE_URL}/<svc>/api/v1/_query`
- required scope = `query.read.<svc>` (must be in Orion's own token `paths`)

If a richer mapping is wanted later (logical name ≠ svc, explicit scope override),
use a JSON env instead:
```
ORION_DATASOURCES={"truth":{"svc":"truth","scope":"query.read.truth"}}
```
**Recommend the CSV form for #85** (simplest, no JSON-in-env). Flag to Eleven if
JSON is preferred.

**Compile-time validation:** a `db.query` node referencing a DataSource not in
`ORION_DATASOURCES` fails compile with `DATASOURCE_NOT_DECLARED` (structural, not a
capability rejection — `db.query` stays fully served).

---

## 2. Async completion (B-syswrite) — #85 path + #86 reuse

### 2.1 Two completion channels, only ONE is HTTP

- **Intra-process (http.request / source.read / db.query — #85):** the effect runs
  on the worker pool; on return the worker delivers `scene.Input(InputMsg{ResumeExec: <wakeKey>})`
  **in-process** (`Orion/internal/runtime/scene.go:518-519`). NO HTTP endpoint, NO
  auth — the resume travels the inbox like any internal control message. The
  wake key is version+epoch-stamped (`wk|<SceneVersion>|<epoch>|<seq>`,
  `exec_timer.go:104-106`); `resumeParked` already drops stale ones.
- **External report (animation.play `completed` — #86):** the renderer (Solar/CEF
  in Pulsar) reports over HTTP. THIS is the dedicated authenticated endpoint.

> **⚠ ADR 011 I3 amendment (the wake-key channel changed shape).** This section
> originally had the renderer **learn** its `wake_key` by reading the OBJECT leaf
> `__anim.<overlay>.<gen>` (which carried `animation_id`/`params`/`generation`/
> `wake_key`). ADR 011 I3 **removes that object leaf** — the live trigger is now
> the SCALAR leaf `__anim.<overlay>` = a bare `uint64` generation counter (LSDP
> §3.2.1 scalar-only). `animation_id`/`params` are resolved at COMPILE time into
> the keyframe node and leave the wire entirely; the `wake_key` is kept
> **server-side** (park map + timer fallback) and is no longer broadcast on any
> leaf. **Consequence: there is currently NO wire channel by which an external
> renderer can learn the `wake_key`.** This is acceptable today and the
> replacement channel is deferred — see §5 (the obj→scalar reconciliation and the
> wake-key decision). The §2.2 body below describes the *target* completion
> endpoint shape; the renderer-learns-wake-key clause is superseded by §5.

### 2.2 Completion endpoint (exact)

`POST /api/v1/scenes/{scene_id}/exec/completion` (reached as
`POST /orion/api/v1/scenes/{scene_id}/exec/completion` via ZabGate).

**Body:**
```json
{
  "wake_key": "wk|sha256:<sceneVersion>|<epoch>|<seq>",
  "kind": "animation",
  "result": { "...": "renderer-supplied completion payload, optional" },
  "error": null
}
```
- `wake_key` — opaque to the renderer; it echoes the key Orion handed to the
  renderer when the animation started. The renderer must carry it through and
  never mints one. **(SUPERSEDED by ADR 011 I3 — see §5.)** The original source
  of this key was the object leaf `__anim.<overlay_id>.<token>`, which I3 removed;
  the leaf is now the scalar generation counter `__anim.<overlay>` and carries no
  `wake_key`. No replacement learning channel is wired today (the report path is
  R9-dormant, #87). **TODO before any prod use of the external report:** specify a
  scalar, §3.2.1-compatible wake-key channel — see §5.2.
- `kind` — `"animation"` for #86 (only external kind for now).
- `error` — non-null string ⇒ the continuation resumes down the effect's `error`
  output port (effect semantics, never a crash).

**Response:** `202 Accepted` on a legitimate resume; `202` ALSO on a dropped
forged/stale report (do not leak which) with a counted metric. Never 4xx that
reveals continuation existence. (Confirm 202-vs-204 with Vigil; 202 recommended.)

### 2.3 Completion scope string (exact)

Carried by `X-Authenticated-Paths`, enforced by the EXISTING `CanWritePath`
(`identity.go:82`). It IS an Orion-leaf-namespace scope (unlike `_query`), because
the report resumes a continuation that conceptually writes the `__system.*`
completion. Use:

```
__system.anim.report
```
(matches the ADR's example verbatim, §3.1.3 / criterion #19). The renderer's
service token carries `paths = ["__system.anim.report"]` (plus whatever else it
needs). This is a **service-token scope, NOT a new JWT role** (Amendment 1 §2).

**Who holds it / how minted:** the rendering client (Pulsar CEF / Solar host) gets
its service token from `POST /auth/api/v1/service-tokens` with
`{"service": "pulsar-cef", "paths": ["__system.anim.report"], "ttl_s": 3600}`.
Operator-minted. ZabGate propagates it as `X-Authenticated-Paths` on the REST
proxy — **once §0 lands**.

### 2.4 Ordered gates (confirmed — implement in this order)

For each external completion report, Orion's endpoint handler runs, in order:

1. **scope** — `identity.CanWritePath("__system.anim.report")` (role=service +
   path match). Fail ⇒ drop+count, 202.
2. **scene match** — `{scene_id}` in the path identifies the target scene; route
   to exactly that scene. A wake key for a scene other than `{scene_id}` never
   resolves (the parked map is per-scene). No cross-scene resume.
3. **version+epoch** — delegated to the existing `resumeParked` inner gate
   (`exec.go:418-430`): `parseWakeStamp` + compare `SceneVersion`/`execEpoch`.
   Stale ⇒ `ExecResumeStale`, drop.
4. **parked continuation match** — `resumeParked` looks up `execParked[key]`;
   unknown key ⇒ drop+log, resumes nothing.

The endpoint adds gates (1)+(2) ABOVE the existing `resumeParked` (which already
does (3)+(4)). Delivery into the scene goroutine MUST go via
`scene.Input(InputMsg{ResumeExec: wake_key})` (cross-goroutine, FIFO) — never a
direct call.

### 2.5 Server-side duration fallback (animation, #86)

When no rendering client is attached (broadcast viewers cannot input),
`animation.play` `completed` fires off the **timer wheel** (`exec_timer.go`) using
the authored/declared animation duration — same park/resume path, internally minted
wake key. If both the timer AND an external report fire, the duplicate-key drop
(`parkTask` "duplicate_key", `exec.go:391-398`) makes the second a no-op — idempotent.

### 2.6 inbox.go hardening (#85 must close this)

The B-syswrite hole in `Orion/internal/adapters/inbox.go`:
- L78-83: `if !w.System { CanWritePath check }` — a write with `System=true`
  bypasses the scope check entirely.
- L166-168: `sceneAcceptsPath` returns `true` unconditionally for any
  `__system.*` path (the tick fan-out shortcut).

Together: a forged inbound `__system.*` write with `System=true` could fan out to
every scene and (pre-phase-3) resume continuations. **#85 must ensure no
externally-sourced write can set `System=true` or reach `__system.*` as a free
write.** Concretely:
- `System=true` is set ONLY by Orion-internal sources (tick, worker-pool
  completion, timer wheel) — never derivable from an inbound HTTP/WS request. Audit
  every call site that constructs `adapters.Write{System: true}` and assert it is
  server-trusted.
- Async completions resume via the **dedicated endpoint + `ResumeExec`** (§2.4),
  NOT via a `__system.*` state write. After #85, a raw `__system.*` write can no
  longer resume any continuation (criterion #19).
- Keep the `__system.*` tick fan-out shortcut (L166-168) ONLY for genuinely
  internal `System=true` tick writes; an external `__system.*` write is rejected at
  L78-83 because an inbound identity never legitimately holds a `__system.*` write
  scope unless explicitly granted (and `__system.anim.report` is consumed by the
  endpoint, not by a free inbox write).

---

## 3. Sequencing & cross-repo prerequisites

### Order
- **#85 first:** worker pool + wake keys; shared completion path
  (`ResumeExec` intra-process); inbox.go hardening; `http.request` egress;
  `db.query` topology-A client + `ORION_DATASOURCES` + `DATASOURCE_NOT_DECLARED`.
- **#86 next:** `animation.play` + the external completion HTTP endpoint (§2.2) +
  duration fallback. Reuses #85's park/resume + the same `resumeParked` gates.

The external completion endpoint logically belongs with #86 (animation is the only
external reporter). #85 needs NO HTTP completion endpoint — its completions are
intra-process. Confirmed.

### Cross-repo prerequisites
| Prereq | Repo | Blocks #85? | Blocks #86? |
|---|---|---|---|
| **P1** — ZabGate injects `X-Authenticated-Paths` on REST proxy for `role=service` (§0) | ZabGate | Blocks the `_query` E2E (runtime enforcement) — NOT the Orion code | Blocks completion E2E |
| **P2** — `_query` enforces the service-token scope `query.read.<svc>` (§1.2) | ZabTruth, ZabRanking (+any DataSource owner) | Blocks the `_query` E2E — NOT the Orion code | no |
| **P3** — ZabAuth provisioning: mint Orion token w/ `query.read.*` + renderer token w/ `__system.anim.report` | ZabAuth (operator config, no code) | config-only | config-only |
| **P4** — scope-string granularity decision (`query.read.<svc>` vs per-datasource) | Atlas/Eleven | decision, not code | no |

**Can #85 start? YES** — the Orion-side build (worker pool, wake keys, inbox
hardening, egress, `db.query` client, `ORION_DATASOURCES`) has no upstream code
dependency. P1+P2 are required for the **E2E smoke test to pass** (and thus for
merge under the Resolution criteria), so they must be opened as parallel issues now
and land before the phase-3 merge. P4 (one decision) should be made before Forge
writes the scope strings — it's a 1-line choice, recommend `query.read.<svc>`.

---

## 4. Bastion phase-3 re-clearance scope (NOT done here — Bastion at merge)

- **R2/B1 egress (veto maintained):** default prod mode of the `http.request`
  host/scheme allowlist + post-DNS anti-SSRF resolution (deny RFC1918/metadata,
  https-only). Conduit does not clear this.
- **`_query` wire:** the new service-side scope enforcement (P2) + Orion's token
  scope surface — Bastion reviews that an undeclared/over-broad `paths` cannot widen
  read access; that `_query` stays read-only.
- **Completion wire:** the dedicated endpoint + the 4 ordered gates + the inbox.go
  hardening (System bypass + `__system.*` free write closed) — Bastion confirms a
  forged/stale/cross-scene completion resumes nothing (criterion #19).
- **P1 ZabGate REST `X-Authenticated-Paths` injection:** auth-surface change →
  Bastion clearance (it widens what a service token can do over REST).

---

## 5. ADR 011 I3 — `animation.play` wire shape: object → scalar (Conduit I5)

> Reconciliation of the `__anim` wire leaf for ADR 011 (animation keyframe
> lowering). Proven 2026-06-13 against **both sides of the real code**, not the
> ADR prose: Orion emitter (`internal/runtime/exec_anim.go`,
> `internal/compiler/lower_animation.go`) and Solar consumer
> (`Solar/src/overlay/animation.ts` `buildAnimationNode` + `@lumencast/runtime`
> `KeyframePlayer`). All `__anim` consumers grepped across Orion / Solar / Prism /
> Pulsar — see §5.3.

### 5.1 Wire shape — before → after

| | **Before (ADR 003 Amd 1, §2.1/§2.2)** | **After (ADR 011 I3)** |
|---|---|---|
| Leaf path | `__anim.<overlay>.<gen>` (object) | `__anim.<overlay>` (scalar) |
| Leaf value | JSON object `{animation_id, params, generation, duration_seconds, wake_key}` | bare `uint64` (the per-scene monotone generation counter, e.g. `7`) |
| `animation_id` / `params` | on the wire | **resolved at COMPILE time** into the lowered keyframe `RenderNode` (`lower_animation.go` `buildAnimationNode`); leave the wire |
| `duration_seconds` | on the wire | **server-side only** — arms the timer-wheel fallback; never needed the leaf |
| `wake_key` | on the wire (renderer learned it here) | **server-side only** — park map + `resumeParked`; not broadcast (see §5.2) |
| LSDP §3.2.1 (scalar-only) | **violated** (object leaf is filtered off the wire) | **passes** — a bare `uint64` is scalar; the M9 replay trigger Solar's `KeyframePlayer` keys on |

The scalar leaf is the SOLE live signal: a value change at `__anim.<overlay>`
remounts the `KeyframePlayer` and replays the compile-resolved geometry — the
same proven M9 reactive path `wipe-cover` already uses. This is what makes the
animation visible on the wire (the prior object leaf was silently dropped by the
§3.2.1 scalar filter — black-screen class of bug, cf. #132).

**Emitter ↔ consumer parity:** `exec_anim.go` writes
`__anim.<overlay>` = `strconv.FormatUint(gen,10)`; the compiler binds the
lowered keyframe node's `keyframes.key` to the same `__anim.<overlay>`
(`lower_animation.go`); Solar's `buildAnimationNode` sets `keyframes.key =
leafPath` byte-identically (the Go↔TS parity oracle, ADR 011 §3.3/D6). Contract
coherent on both sides.

> **⚠ Cross-repo merge-order flag for Eleven (deployment reality, 2026-06-13).**
> The two sides are NOT both deployed yet — they are **out of step on `main`**:
> - **Solar `main`** already carries the **scalar** consumer (`buildAnimationNode`,
>   #22 merged, `cc4a96c`).
> - **Orion `main`** still emits the **object** leaf `__anim.<overlay>.<gen>`
>   (`exec_anim.go:119` on `main`); the scalar emitter + `lower_animation.go` live
>   only on the **unmerged** branch `forge/orion-solar-adr011-animation-core`
>   (I2/I3/I4 — commits `405068a`/`a1db6d1`/`4e5396e`), NOT on `main`.
>
> So the brief's premise "I2/I3/I4 mergé+déployé (Orion #161)" does not match the
> repo: **#161 is not on Orion `main`.** This mismatch is **inert today** because
> the whole animation exec path is R9-dormant (no prod scene installs an
> ExecProgram until #87, ADR 011 §3 / ADR 006) — no live scene exercises either
> leaf shape. But Solar's oracle and Orion's live emitter currently disagree
> byte-wise. **Resolution:** Eleven merges the Orion animation-core branch (the
> emitter) to converge `main` with the already-merged Solar consumer. Producer
> (Orion emitter) and consumer (Solar oracle) are not rétro-incompatible at run
> time only because both are dormant; once #87 lifts dormancy the Orion branch
> MUST be on `main` first. This contract describes the **post-branch-merge** shape
> (the agreed target), and is the binding spec both sides are pinned to.

### 5.2 Wake-key channel for the external report — DECISION

**Decision: (a) — nothing to wire now; declare the channel dormant + a TODO gate.**

Rationale (proven, not asserted):

1. **No live regression, no current consumer.** The external-report path (#86) is
   **R9-dormant**: no production scene installs an ExecProgram until #87, and
   **Solar/CEF carries no completion-reporting code today** (grepped: zero
   `wake_key` / `exec/completion` references in Solar/Prism/Pulsar — §5.3). The
   sole resolver in use is the **server-side duration fallback** (timer wheel,
   `exec_timer.go`), which is **leaf-independent** — it keys off the server-held
   park map, never the `__anim` leaf. Removing the object leaf removes a channel
   that **nothing reads**.
2. **No speculative architecture.** Forge correctly did NOT invent a sidecar
   wake-key channel (platform doctrine: no speculative wiring). Specifying a
   replacement scalar channel **now**, with no consumer and no report code to
   exercise it, would be unfalsifiable contract — it could not be proven by a
   smoke test, only asserted. A contract clause that no running code exercises is
   exactly what this doc forbids ("a contract is proven, never deduced").
3. **The ADR is consistent under this reading.** ADR 011 §3.6 says completion is
   "unchanged in mechanism / resolves exactly as today" and "**no new Bastion
   surface**". That holds precisely *because* the only live resolver (duration
   fallback) is leaf-independent. The object leaf was never the *mechanism* of
   completion — it was an incidental **learning channel** for a renderer that does
   not yet exist. Dropping it does not contradict §3.6; it just retires an
   unbuilt channel. No ADR amendment is required for the dormant state.

**TODO gate (binding, before any prod use of the external report):** when #87
lifts R9 dormancy AND a reporting renderer is built, a wake-key learning channel
MUST be (re)specified here. Constraints fixed now so the future channel stays
in-contract:
- **scalar, §3.2.1-compatible** — it may NOT reintroduce an object leaf on the
  LSDP wire (that is the black-screen regression class).
- Candidate shapes (NOT decided — to be chosen with the renderer's real needs):
  a sibling scalar leaf `__anim.<overlay>.wake` carrying the opaque string, OR
  delivery of the wake_key in the render-bundle/start-of-animation handshake
  rather than as a live delta. Either keeps the key off the object-leaf path.
- Picking among them is a **wire-shape** call (Conduit owns it) *unless* it
  touches the completion auth contract or the LSDP envelope grammar — in which
  case it is an **ADR 011 amendment → Atlas via Eleven (gated)**. Flagged here so
  it is not slipped in silently.

This decision does **not** require Atlas now: it changes no architecture, adds no
mechanism, and matches ADR 011 §3.6 as written. It only records, in-contract, a
gap that ADR 011 §3.6 left implicit (the §3.6 prose is silent on the wake-key
*learning* channel — it speaks only of the resume *mechanism*).

### 5.3 Consumers verified (the "both sides, all consumers" proof)

Grepped across `Orion/`, `Solar/src`, `Prism/src`, `Pulsar/` (non-test, non-vendored):
- **Emitter:** `Orion/internal/runtime/exec_anim.go` (scalar write, branch),
  `Orion/internal/compiler/lower_animation.go` (keyframe-node key bind, branch).
- **Consumer:** `Solar/src/overlay/animation.ts` `buildAnimationNode` +
  `@lumencast/runtime` `KeyframePlayer` (keys on the scalar leaf). Re-exported via
  `Solar/src/index.ts`.
- **External-report / wake-key consumers:** **none.** Zero `wake_key` /
  `wakeKey` / `exec/completion` references in Solar/Prism/Pulsar runtime code →
  removing the object leaf breaks no consumer.
- **Prism `animation_id` is NOT a wire consumer (verified, false positive).**
  `Prism/src/main/animation-bridge.ts` `AnimationPlayEnvelope` =
  `{overlay_id, animation_id, params}` is the **authoring-side** Blue
  `prism.canvas` SideEffect envelope (mirrors `Blue/src/blue/schemas/
  from_trigger.py`), delivered over Prism's internal `animation:play` IPC — NOT
  the Orion `__anim` LSDP leaf. Prism's only Orion-wire consumer is
  `broadcast-engine.ts` subscribing to `/show/stream.lsdp`, which forwards deltas
  to Solar and never parses `__anim` itself. The `animation_id` field belongs to
  the authoring/asset surface that I3 *deliberately* moves to compile time — it is
  unaffected by the leaf scalarisation and is not a broken consumer.

### 5.4 Coherence with I6 (seed) and I7 (live proof)

The scalar leaf **`__anim.<overlay>`** is the single binding point downstream:
- **I6 (Blue seed):** the `core.animation.play@1` blueprint, once exec-active,
  causes Orion to write `__anim.<overlay>` on each play. The seed harness must
  drive an `overlay_id` whose lowered keyframe node is bound to that exact leaf.
- **I7 (live proof):** the animation harness **binds `__anim.<overlay>`** as the
  scalar whose value-change proves movement at the antenna — each increment of the
  `uint64` generation is one replay of the authored geometry through Solar's
  KeyframePlayer, observable on the Twitch output (the live-testing.md objective
  measure: spatial stddev / luma on the encoded `.mp4`, not a CEF screenshot).

**Confirmed:** `__anim.<overlay>` (scalar uint64) is THE anchor leaf I7 binds to
prove the animation on air. No object leaf, no `animation_id`/`params` on the wire.
