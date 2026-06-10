# ADR 003 — Full Blue execution engine (exec + dataflow) + scene-validation gate + platform-event ingestion

- **Status**: proposed
- **Date**: 2026-06-10
- **Decided**: —
- **Deciders**: @ClodoCapeo
- **Author**: Atlas (architect agent)
- **Supersedes**: ADR 003 « dataflow-only runtime » (accepted 2026-06-10, **reversed
  by the maintainer the same day**, reverted from main in #77 — the file no longer
  exists in-tree; its full text is in git history at `9b841fd`)
- **Superseded by**: —

---

> **Numbering.** The revert of the dataflow-only ADR freed the 003 slot; per-repo
> convention (ADR 001 header note) the next committed number is 003. The
> `../docs/adr/004-orion-v2-runtime.md` referenced by `CLAUDE.md` is a design
> reference that was never committed (no collision — ADR 001 documents this).

## 1. Context

### 1.1 The reversal, and the doctrine this ADR is built on

The previous ADR 003 resolved the engine audit's blocking break #1 (exec pins
accepted then silently ignored) by **rejecting the exec sub-language at
compile**. It was accepted, then reversed by the maintainer within hours. The
reversal is doctrine, and every line of this ADR is constrained by it:

1. **Orion serves the entirety of the Blue engine — no gap, no deviation, no
   simplification, no detour.** That is THE acceptance criterion. A loop authored
   tomorrow works. A 20 000-node operation tomorrow works. Exec pins, `branch`,
   `sequence`, `gate`, `for-loop`/`for-each`/`while`, `delay`, `variable.set`,
   all ~70 Blue node types and the 14 `quasar.*` nodes are served on air. The
   engine's architecture adapts to the language — never the inverse. **Rejecting
   a node at compile is not an option**: the previous ADR's semantic error
   ("cleanly rejecting what we don't handle" = "not handling things we should")
   must not recur.
2. **Orion does not concern itself with whether a primitive is bounded.**
   Boundedness/termination is an *authoring* problem. No safety-by-amputation.
3. **A scene must not be pushable-and-exploitable live unless all of its Blue
   logics are safe and proven.** Safety lives in a **scene-validation gate**:
   validating a scene = testing all of its Blue logics; a failing logic = an
   unvalidated scene = no antenna. We reject/mark *defective scenes*; we never
   limit *capabilities*.

### 1.2 Technical state (verified in code, 2026-06-10)

- **Runtime**: one goroutine per scene (`scene.go:189-200`), single-writer state,
  loop = drain inbox → dirty-driven topo recompute of pure computes → emit delta.
  No exec pins, no loops, no timers (beyond the global tick), no branches, no
  mutable variables, no effects. Registry: 13 computes (`compute.go:41-78`).
- **Silent-ignore hole**: an unregistered compute is logged and skipped at
  recompute (`scene.go:341-345`); the compile gate checks manifest presence +
  purity only (`compile.go:492-574`) and rejects every impure node
  (`IMPURE_COMPUTE`) — i.e. `delay`, `variable.set`, `http.request`, `db.query`,
  `source.read`, `animation.play`, `print` cannot reach the runtime at all.
- **Wiring**: edge `to_port` names are not carried into the artefact;
  `gatherInputs` wires positionally onto `a..d` modulo 4 (`scene.go:367-377`).
  `upstreamPath` linear-scans `graph.Nodes` per lookup (`scene.go:382-392`) —
  O(N²) per recompute pass; unusable at 20 k nodes.
- **Blue's exec semantics** (`stdlib_seeder.py`): UE-Blueprint model — exec vs
  data pins validated separately; latent nodes (`delay` suspends, `then` fires
  later); stateful nodes (`gate` holds open/closed across events); graph
  variables (`variable.get/set`); `animation.play` fires `then` immediately and
  `completed` when the tween ends; `sequence` fires `then_0..2` in order;
  loops expose per-iteration data pins (`index`, `element`).
- **Purity map** (`node_purity.py`): stays the single source of truth — but its
  consumer role changes (§3.1.2): from acceptance gate to **scheduling
  partition** (pure → dataflow layer; impure/exec → exec layer).
- **Test sessions** (`runtime/test_session.go`): isolated cloned scenes already
  exist — the natural substrate for the validation harness (§3.2).
- **Platform ingestion**: Quasar's push client (`orion_client.py`, service token,
  reconnect buffer), Orion's scoped-write enforcement, and the
  `__inputs.platform.*` namespace are built. The compiler binds nothing
  (`nodeLeafPath` ignores `quasar.*`), and an unbound write is silently absorbed
  by `Inbox` (`sceneAcceptsPath`). The reverted ADR's Decision B — including the
  Bastion-required hardening E1/E2/E3 and the Vigil-required synthesized
  `platform-stream` binding — solved this correctly and **restricts nothing in
  the language**; it is re-adopted here (§3.3).

### 1.3 Honest feasibility statement

This is the largest runtime work Orion has taken: an imperative, suspendable,
effectful execution layer integrated with the reactive dataflow substrate,
plus a proof harness, with a 20 k-node performance target. It is feasible —
the single-goroutine-per-scene model and the compiled-artefact pipeline are the
right substrate — but it is **multi-phase work measured in weeks, not days**.
The phases below are independently shippable; **no phase descopes the target**.
The transitional state (a node type whose executor has not landed yet) is
tracked by an explicit, CI-ratcheted shrink-to-zero list (§6, criterion 1) —
never by silent ignore, and the end state is **zero** unserved node types.

## 2. Decision drivers

- **Doctrine §1.1 — completeness is the acceptance criterion.** Architecture
  serves the language. The only acceptable end state is 70/70 core + 14/14
  platform node types executing on air.
- **Determinism and the single-writer model are Orion's asset.** One goroutine
  owns a scene's state; deltas are causally ordered. The exec layer must run
  *inside* that model, not beside it with locks.
- **Safety at the gate, not in the engine.** The engine executes `while(true)`
  faithfully; the validation gate is where a diverging logic is caught — before
  air, by proof, per scene.
- **Fail-loud over fail-silent, relocated.** The dead accept-then-ignore class
  must stay dead — but the loudness moves from "reject at compile" to
  "conformance is tested per node type, transitional gaps are an explicit
  ratcheted list, and on-air execution is fully observable".
- **Don't rebuild what exists**: dirty-driven dataflow, test-session isolation,
  Quasar's push path, ZabAuth scoped tokens, the M9 trigger→repaint model.
- **20 k nodes is a stated operating point**, not a stretch goal: data-structure
  choices (indexes, dirty-cone walks) are made for it from phase 0.

## 3. Decision

**Verdict: GO.** Orion gains a **hybrid scheduler**: the existing dirty-driven
dataflow substrate (pure data nodes, incremental, memoized by state) plus an
**exec-task interpreter** (events fire resumable tasks that walk exec chains,
perform effects, suspend on latent nodes) — both running on the scene's single
goroutine. Safety moves to a **scene-validation gate** owned by Orion. Platform
ingestion re-adopts the reverted ADR's push design with its Bastion hardening.

### 3.1 Decision A — the execution model

#### 3.1.1 Two layers, one goroutine, one state

The compiler partitions each blueprint by pin kind (Blue's signatures declare
`exec` vs `data` ports — `stdlib_seeder.py:51-76`):

- **Data layer** (exists today): pure data nodes wired by data edges. Stays
  dirty-driven and incremental. Evaluation becomes **dirty-cone-only**: a
  reverse-adjacency index + topo-index ordering walks only the affected cone,
  not the full `computeOrder` (O(dirty cone), not O(N) — and `upstreamPath`
  becomes an O(1) map). This is the 20 k enabler.
- **Exec layer** (new): event nodes (`on-start`, `on-tick`, `on-event`) and
  exec-edge chains. An event firing creates an **exec task**: an explicit,
  resumable interpreter state (program counter on the exec chain + value
  environment + continuation stack for nested loops/sequences). Tasks run
  synchronously on the scene goroutine inside the existing loop; **no
  goroutine-per-task, no locks** — determinism and single-writer are preserved
  by construction.

When a task needs a data input (e.g. `branch.condition`), it **pulls** the pure
upstream cone through the data layer (demand evaluation, memoized against
current state — same functions, same registry). When a task performs an effect
(`variable.set`, `print`, leaf write via `output` exec, `animation.play`,
`http.request`, `db.query`), it goes through a single **effect interface** on
the scene — the one seam where the exec layer touches the world.

#### 3.1.2 Purity becomes a partition, not a gate

`IMPURE_COMPUTE` **dies as an acceptance gate** (criterion 18 of the v2
scaffold is superseded). Blue's purity map remains the source of truth and is
consumed as scheduling metadata: `is_pure` → data layer (memoizable);
otherwise → exec layer (effect/latent executor). The compile gate that remains
is *structural only*: malformed graphs (unknown definition, bad ports, exec
edge into data pin — already Blue-validated), never node-type capability.

#### 3.1.3 Exec semantics (UE-faithful, documented)

- **`branch`**: pulls `condition`, continues down `true` or `false`.
- **`sequence`**: runs `then_0`, `then_1`, `then_2` in order; if a child chain
  suspends on a latent node, the sequence continues with the next sibling
  (UE latent semantics) — the suspended continuation resumes independently.
- **`gate`**: persistent per-node state (open/closed), stored in scene state
  under `__nodestate.<node_id>` (reseeded from `start_closed` on restart —
  criterion 11 of the scaffold holds). `enter` passes through iff open.
- **`for-loop` / `for-each` / `while`**: iterate inline, pushing a loop frame
  on the task's continuation stack; per-iteration pins (`index`, `element`)
  are bound in the task environment and readable by the body's data pulls;
  `completed` fires after the last iteration. **No iteration cap.** Fairness is
  by **time-slicing**: after a step budget (default 10 000 steps or 4 ms,
  env-tunable), the task yields — its continuation is re-enqueued on the scene
  loop, pending inputs are drained, a delta is emitted, then the task resumes
  *exactly where it stopped*. Preemption changes scheduling, never semantics:
  no kill, no skip, no amputation. (On-air starvation is a non-issue *because*
  the validation gate proves termination-within-budget before air — §3.2.)
- **`delay`**: latent. The task parks with a wake key on the scene's **timer
  wheel** (a new `time.Timer`-driven channel in the scene loop's select); on
  fire, the continuation resumes on the goroutine. Tests use a fake clock.
- **`variable.get/set`**: graph variables are state leaves under
  `__vars.<blueprint_key>.<name>` (namespaced per ADR 001 §3.3 like blueprint
  leaves). `set` writes through the effect interface → marks dirty → the data
  layer and subscribers react; `get` is a pure data node reading the leaf.
  Sets apply in task order; deltas batch at task yield/completion points.
- **`print`**: structured log + write to `__debug.<blueprint_key>.print` (ring
  of last N lines) so authors see it in test sessions.
- **`on-start`**: fires when the scene becomes live (activation / push-swap)
  and on each test-session open. **`on-tick`**: subscribes the scene to the
  global tick (`tick.go`), binding `delta_seconds`. **`on-event`**: fires on a
  write to `__events.<event_name>` (operator/service-dispatched topic).
- **Async effects** (`http.request`, `source.read`, `db.query`,
  `animation.play` `completed`): the effect executes off-goroutine (bounded
  worker pool, per-effect timeout as *effect semantics*, with the `error`
  output port carrying failures); the task parks on a wake key; completion is
  delivered as an inbox message and the continuation resumes. `db.query`
  executes the compiled `QueryDescriptor` (Blue's plan-builder atomics) via
  pgx against a declared DataSource — the DataSource resolution contract
  (which DB, credentials from étage 1) is a **Conduit contract task**
  (phase 3 issue), not an open question about whether it ships.
- **`animation.play`**: emits the animation command as a state write
  (`__anim.<overlay_id>.<token>` carrying `animation_id`, `params`,
  generation counter) so it travels the normal delta pipe and is rendered by
  Solar/CEF — per the platform doctrine, animations are rendered by our
  engine, never OBS-native. `then` fires immediately; `completed` resumes on
  the renderer's completion report (`__system.anim.<token>.completed`, a
  scoped system input from the control-mode client) with a server-side
  duration-based fallback when no reporting client is attached (broadcast
  viewers cannot input). The exact report contract is a **Conduit task**
  (phase 3).

#### 3.1.4 Interruption / cancellation

Scene switch-away, re-push, archive and shutdown **cancel all live tasks** of
the affected scene version: parked wake keys are dropped, timer entries
cleared, in-flight effect results discarded on arrival (version-stamped wake
keys). A re-pushed scene starts from defaults + `on-start` — consistent with
the restart-reseed model.

#### 3.1.5 20 k-node performance design

- O(1) node/leaf lookup maps and reverse adjacency, built at scene load.
- Dirty-cone recompute (topo-index ordered queue) — cost proportional to the
  affected subgraph, not the scene.
- Cold-start full pass stays (once, at load) — budget ≤ 2 s at 20 k nodes.
- Steady-state budget: input→delta p95 ≤ 50 ms for dirty cones ≤ 1 000 nodes
  (the existing guarantee, restated at scale); a synthetic 20 k benchmark
  graph lives in the repo and runs in CI (regression-gated).

#### 3.1.6 Alternatives rejected

- **Reject exec at compile** (the reverted ADR): excluded by doctrine §1.1 —
  not re-argued, not weakened, not re-proposed in any form.
- **Goroutine-per-task with channel effects**: simpler to write, but ordering
  between concurrent tasks and the dataflow loop becomes racy/non-deterministic
  and untestable at 20 k scale; rejected for the explicit-continuation
  interpreter on the scene goroutine.
- **Separate exec service / process**: a network hop inside the ≤ 50 ms loop,
  shared-state coherence problems, a deploy for zero capability. Rejected.
- **Compile exec idioms down to pure dataflow**: unfaithful in general
  (`sequence`/`gate`/`delay` *are* ordering and time); rejected as the primary
  model. (Nothing prevents the data layer from staying the fast path for pure
  cones — that is the hybrid.)

### 3.2 Decision B — the scene-validation gate (Orion-owned)

**Verdict: the gate lives in Orion.** Only Orion can execute Blue logics, and
only Orion controls air; the prover and the enforcement point must be the same
system or the proof is theatre. Prism/Canvas surface the status (consequences,
cross-referenced); a follow-up authoring-UX ADR in Prism is flagged but not
required for enforcement.

#### 3.2.1 What "all logics tested and proven" means, concretely

A **validation campaign** runs against a pushed `(scene_id, scene_version)`,
in an **isolated validation session** (the `TestSessionManager` clone substrate,
extended): private state, no live subscribers, **no external side effects** —
the effect interface runs in *validation mode*: `http.request`/`db.query`/
`source.read` execute against recorded fixtures or return declared-shape
synthetic responses (and the report lists every attempted call);
`animation.play` completes via its duration fallback; `print` captures.

For **each blueprint** of the scene, for **each event entrypoint**
(`on-start`; `on-tick` × N frames; each `on-event` topic; each `quasar.*` node
fed the canonical fixture for its event type; each operator input at default +
boundary values from its declared type):

- the harness fires the entrypoint and the engine executes it **with the same
  interpreter as live** (no separate semantics to drift);
- **pass** requires: the fired task terminates within the validation budget
  (default 5 s wall / 1 M steps per entrypoint, env-tunable), zero compute
  errors, zero unknown computes, state-size within budget;
- the report (JSONB) records per-entrypoint: steps, wall time, leaves written,
  effects attempted, exec-node coverage (informative — uncovered arms are
  listed, not failing: a branch arm legitimately unreachable under fixtures is
  the author's call to inspect).

A scene passes iff **every entrypoint of every blueprint passes**. One failing
logic = unvalidated scene. The budgets bound the *proof*, never the engine: a
`while` that diverges fails validation and never reaches air — the language
lost nothing.

#### 3.2.2 Status, enforcement, invalidation

- New table `scene_validations(scene_id, scene_version, status
  validated|failed, report jsonb, harness_version, created_at)`.
- `POST /api/v1/scenes/{id}/validate` runs the campaign on the latest pushed
  version (async; `GET /api/v1/scenes/{id}/validation?v=` returns status+report).
- **Enforcement**: `POST /show/active-scene` (and the push-swap path for an
  already-live scene) refuses any version without a `validated` record —
  `SCENE_NOT_VALIDATED`, alongside the existing `SCENE_NOT_PUSHED`. Test
  sessions remain free (that's where authors iterate).
- **Invalidation by construction**: the record is keyed by `scene_version`
  (the artefact hash — blueprints, layout, channel config all participate).
  Any change ⇒ new version ⇒ no record ⇒ not air-eligible. No staleness
  tracking needed; `harness_version` additionally allows fleet-wide
  re-validation when the harness semantics change.
- **Runtime defense-in-depth (observability, not amputation)**: on air,
  time-slice preemptions and budget crossings increment metrics
  (`orion_task_preempt_total`, `orion_task_budget_exceeded_total`) and warn —
  a validated scene behaving pathologically live is an incident signal, never
  an automatic kill.

### 3.3 Decision C — platform-event ingestion (re-adopted from the reverted ADR, unchanged in substance)

The reverted ADR's Decision B hardened the producer and bound the leaves —
it restricted nothing in Blue. Re-adopted wholesale:

1. **No Orion-side platform adapter; Quasar pushes** over its existing
   service-token WS as scoped `input` writes (ADR 005 §9 path).
2. **One leaf convention**, `__inputs.platform.<platform>.<channel>.last_<event_type>`
   (channel casefolded, `[a-z0-9_]+`); **Blue aligns** its default
   `event_alias` to `last_<event_type>` (`_shared.py`), re-stamped by the
   seeder.
3. **Compiler binding + channel expansion**: `quasar.*` manifest entries
   compile to `Kind:"input"`, `Path:<expanded leaf>` from the node's authored
   `config.channel` (missing → `PLATFORM_CHANNEL_MISSING`, invalid →
   `PLATFORM_CHANNEL_INVALID` — *structural* validation of authored config,
   not capability rejection), **and a synthesized
   `ExternalAdapter{Kind:"platform-stream"}` binding in `graph.Bindings` per
   distinct leaf** so `sceneAcceptsPath` accepts the write (normative — the
   Vigil-verified fix; without it Quasar's writes are silently absorbed). No
   adapter goroutine is ever spawned for it. Leaf value = whole canonical
   event; extraction via `core.data.get-field` (data layer) and/or exec
   reaction via the M9 dirty-trigger path.
4. **Bastion hardening, all in scope** (acquired clearance basis):
   **E1** producer-side fail-closed `leaf_path` validation in Quasar (platform
   enum, type ∈ `CANONICAL_EVENT_TYPES`, channel `^[a-z0-9_]+$`, drop+log
   before any send); **E2** per-leaf chat coalescing in Quasar
   (`QUASAR_ORION_COALESCE_MS`, default 100 ms, lossless under latest-value) +
   `orion_inbox_dropped_total` consuming `scene.Input`'s return in
   `Inbox.Write`; **E3** token scope `__inputs.platform.twitch.*` only;
   recommended reconnect-buffer drop counter.
5. **Event semantics**: latest-value last-write-wins per leaf, FIFO per
   connection, at-most-once across reconnects, no replay; restart reseeds
   defaults. Stateful aggregation over event streams (sub counters, hype
   trains) is served by the exec layer + variables once phase 1 lands —
   an *upgrade* over the reverted ADR, which had to defer it.
6. **Cross-repo leaf contract test** (Blue ↔ Quasar shared fixture,
   byte-equality over all 14 event types) in both CIs.

### 3.4 Phases (each shippable; none descopes; target is 100 %)

| Phase | Contenu | Repos |
|---|---|---|
| **0 — Fondations** | Named ports in the artefact (edge `to_port` carried; positional fallback for old artefacts); O(1) indexes + dirty-cone recompute; 20 k synthetic benchmark in CI; pure-registry tranche (logic, extended math, string, cast, data) | Orion |
| **1 — Moteur exec** | Task interpreter (branch/sequence/gate/loops/select), continuation stack, time-slicing; timer wheel + `delay`; `variable.get/set`, `print`; triggers `on-start`/`on-tick`/`on-event`; cancellation on switch/re-push | Orion |
| **2 — Ingestion plateforme** (parallélisable avec 1) | §3.3 : binding compilateur + `platform-stream` ; E2 côté Orion ; E1+E2+E3 côté Quasar ; alias + re-stamp côté Blue ; contract test cross-repo ; E2E ≤ 100 ms | Orion · Quasar · Blue |
| **3 — Effets asynchrones** | Worker pool + wake keys ; `http.request`, `source.read`, `db.query` (DataSource contract → Conduit) ; `animation.play` + completion contract (→ Conduit) | Orion (+Conduit) |
| **4 — Gate de validation** | Harness + validation mode of the effect interface ; `scene_validations` ; endpoints ; `SCENE_NOT_VALIDATED` enforcement ; fixtures canoniques | Orion |
| **5 — Conformité totale** | Conformance matrix CI (manifest ↔ executor ↔ test, allowlist ratchet → **vide**) ; 20 k perf criteria green ; doc resync (CLAUDE.md, runbooks → Scribe) | Orion |

Gate enforcement (phase 4) becomes mandatory for go-live as soon as it lands;
phases 1–3 grow what the harness can prove. The conformance allowlist exists
from phase 0 and **only shrinks** (CI ratchet); phase 5 ends with it empty.

## 4. Consequences

- The accept-then-ignore class stays dead — by **execution**, not rejection:
  what compiles runs; what hasn't shipped yet is a named, CI-ratcheted,
  shrinking list; what is defective is caught by the validation gate per
  scene, before air.
- `IMPURE_COMPUTE` rejection and scaffold criterion 18 are superseded; purity
  becomes the layer partition. Blue's validator and registries are untouched
  (the language was never the problem).
- The scene goroutine model, restart-reseed (criterion 11) and the delta
  pipeline are preserved; the ≤ 50 ms guarantee is restated as a dirty-cone
  budget at 20 k scale.
- A Twitch-reactive, loop-driven, animated overlay becomes authorable
  end-to-end — including stateful event aggregation (variables + exec), which
  the reverted design could not offer.
- New operational surface: timer wheel, effect worker pool, validation
  campaigns (CPU-bounded, off the live path), `scene_validations` migration.
- Prism: surfaces validation status/report and the new diagnostics; palette
  no longer needs runtime-support filtering (everything is supported) —
  follow-up UX ADR flagged, non-bloquant.
- Existing pushed scenes are unaffected until re-push; once phase 4 lands,
  going live requires validation — a one-time `validate` call per scene.

## 5. Risks

Security-surfaced risks → **Bastion** (no self-clearance).

- **R1 — Interpreter complexity** (continuations, latent semantics,
  cancellation). Highest engineering risk. Mitigation: explicit task state
  (no goroutine state), fake-clock tests, UE-documented semantics per node,
  phase 1 lands behind the conformance matrix.
- **R2 — `http.request` SSRF / egress** (blueprint-authored URLs executed from
  inside the infra). **Bastion decision required** at phase 3: egress policy
  (allowlist / deny-internal-ranges / proxy) is *deployment policy on the
  effect executor*, not a language restriction. Validation mode performs no
  real calls.
- **R3 — `db.query` credential surface**: DataSource credentials at étage 1,
  resolved by Orion config, never in blueprints/artefacts; read-only roles.
  Conduit contract + **Bastion review** (phase 3).
- **R4 — Path injection via channel/type** (reverted-ADR R2): closed
  fail-closed at both ends (compiler §3.3.3 + Quasar E1). **Bastion
  re-clearance** on the ingestion surface (phase 2).
- **R5 — UGC fan-out** (chat text → subscribers, and now → exec triggers):
  reaches only author-wired leaves (`sceneAcceptsPath` filtering is the
  implementation); rendering safety is Solar/Lumencast escape-by-default;
  exec reactions to UGC are author-authored logic, proven at the gate.
  Residual accepted, documented.
- **R6 — Validation theatre** (fixtures diverge from live traffic; a validated
  scene still misbehaves live). Mitigations: same interpreter for harness and
  live; canonical fixtures shared with Quasar's contract tests;
  `harness_version` re-validation lever; live observability metrics (§3.2.2).
  Residual accepted: validation proves termination/budgets under
  representative inputs, not total correctness — that is the stated contract.
- **R7 — Live starvation by a validated-but-heavy scene**: time-slicing keeps
  the loop responsive; budget-crossing metrics make it visible; no kill.
  Residual accepted per doctrine (no amputation).
- **R8 — At-most-once platform loss windows** (reverted-ADR R5): unchanged,
  accepted, documented — overlays show latest state; ledgers come from a
  queryable source of truth.
- **R9 — Phase risk**: a long transitional window where some node types are
  on the allowlist. Mitigated by the ratchet (never grows), per-phase issue
  ordering, and the gate landing at phase 4 (safety does not wait for
  phase 5 completeness).
- **R10 — http-poll / pg-listen layout adapters still undeclared in prod**
  (`extractAdapters` stub) — out of scope, still tracked against the
  Canvas-extensions chantier.

## 6. Resolution criteria

Testable; CI-enforced where possible. **Criterion 1 is the master criterion.**

1. **Total conformance (master).** A CI job asserts: for **every** node id in
   Blue's compute manifest (the ~70 `core.*` + 14 `quasar.twitch.*`), Orion
   has a registered executor (data-layer compute or exec-layer executor)
   **and** a passing execution test exercising it through the real engine.
   Transitional gaps live only in `conformance_allowlist.txt`; a ratchet test
   fails CI if the list ever grows. **Final state: the list is empty** — every
   valid Blue blueprint is served in full, no exceptions.
2. **No capability rejection.** A test pushes a blueprint containing one node
   of *each* served type: compile accepts; the scene goes live (post-gate:
   after validation passes); every node's effect/value is observable. No
   `UNSUPPORTED_COMPUTE`/`IMPURE_COMPUTE` diagnostic exists for any
   manifest-known node type in the final state.
3. **Exec semantics.** Unit tests per node: `branch` both arms; `sequence`
   sibling ordering incl. latent-child continuation; `gate` open/close/
   start_closed persistence across events and reseed on restart; `for-loop`
   index sequence visible; `for-each` element+index over a list leaf; `while`
   terminating on condition; `delay` fires `then` at +`seconds` (fake clock);
   `variable.set` visible to `variable.get`, the data layer and subscriber
   deltas in task order; shuffled-edge-order wiring correct via named ports.
4. **The maintainer's loop test.** E2E through the real push API: a blueprint
   `on-start → for-loop(0..9) → variable.set(counter=index)` is pushed,
   validated, activated; a subscriber observes `counter == 9`.
5. **20 k nodes.** The synthetic 20 k-node scene compiles, pushes, validates;
   cold-start full evaluation ≤ 2 s; single-input → delta p95 ≤ 50 ms (dirty
   cone ≤ 1 000 nodes); CI benchmark regression-gated.
6. **Preemption is not amputation.** A validated long-running loop crossing
   the time slice: the scene loop keeps serving other inputs during execution
   (interleaved deltas observed), the task completes with a correct final
   state, `orion_task_preempt_total` increments, nothing is killed.
7. **Cancellation.** Scene switch-away mid-`delay` and mid-loop: parked tasks
   are dropped, no late write from a stale version reaches state (version-
   stamped wake keys asserted).
8. **Platform binding bound AND accepted** (reverted-ADR criterion 5 verbatim):
   `quasar.twitch.chat@1` with `config.channel="ZabChannel"` compiles to
   `Path == "__inputs.platform.twitch.zabchannel.last_chat"` + a
   `platform-stream` binding carrying it + `sceneAcceptsPath(...) == true`;
   missing/invalid channel → `PLATFORM_CHANNEL_MISSING`/`_INVALID`.
9. **Cross-repo leaf contract**: for all 14 event types, Blue's declared
   `leaf_path` (expanded) byte-equals Quasar's `leaf_path(event)` — shared
   fixture, both CIs.
10. **Platform E2E**: service-token client sends a canonical chat event; a
    scene wiring `quasar.twitch.chat@1 → get-field → core.output@1` emits the
    delta ≤ 100 ms; out-of-scope write → `WRITE_FORBIDDEN`; restart reseeds
    platform leaves.
11. **Ingestion hardening** (reverted-ADR criteria 9–11 verbatim): E1 hostile
    channel dropped before send (Quasar unit test); E2 coalescing one-push-
    per-window + `orion_inbox_dropped_total` consuming `scene.Input`'s return;
    E3 scope `__inputs.platform.twitch.*` with `WRITE_FORBIDDEN` on
    `youtube.*`.
12. **Gate — divergence caught.** A scene containing `while(true)` validates
    as `failed` (budget exceeded, report names blueprint+entrypoint+steps);
    `POST /show/active-scene` on it → `SCENE_NOT_VALIDATED`; after the author
    fixes and re-pushes, validation passes and activation succeeds.
13. **Gate — invalidation by construction.** Re-pushing any change yields a
    version with no validation record; activation refuses until re-validated.
14. **Gate — no side effects in validation.** A blueprint with `http.request`
    validates without any real egress (asserted via the effect interface in
    validation mode); the report lists the attempted call.
15. **Org gates.** Orion/Blue/Quasar CIs green; review **Vigil** (who flips
    this ADR to `accepted`); **Bastion** clearance on phase 2 (ingestion
    surface) and phase 3 (R2 egress policy, R3 DataSource) before the
    corresponding merges.
