# ADR 011 — `core.animation.play@1` reconciliation: animation-as-authored-keyframe via compiler lowering

- **Status**: accepted
- **Date**: 2026-06-13
- **Decided**: 2026-06-13
- **Deciders**: @ClodoCapeo
- **Author**: Atlas (architect agent)
- **Supersedes**: — (executes the unfulfilled rendering half of ADR 003 §3.1.3
  / §3.4 « `animation.play` … rendered by Solar/CEF off the delta »; amends the
  `__anim.*` object-leaf command shape)
- **Superseded by**: —

---

> **Numbering.** Per the repo convention (ADR 006 header note): 004/005/007 are
> burned by in-tree citations, committed ADRs are 001–003 + 006–010. First free
> number: **011**.

## 1. Context

`core.animation.play@1` is the platform's animation primitive — central to the
M9/M10 vision « animations rendered by OUR engine (Solar/CEF), driven by
blueprint, never OBS-native ». The validation campaign found it **hollow**: it
executes but nothing moves on air. Three facts, re-verified at source
(2026-06-13):

1. **Path B — the exec op exists and is correct** (`Orion/internal/runtime/exec_anim.go`).
   On `in`: increments a per-scene monotone generation, writes the leaf
   `__anim.<overlay_id>.<generation>` carrying an **object**
   (`animation_id`/`params`/`generation`/`duration_seconds`/`wake_key`), fires
   `then` immediately, parks `completed` on the timer-wheel + external-report
   race (the #82/#83/#86 / ADR 003 §3.1.3 Amendment 1 mechanism). This half is
   sound and ships nothing we want to throw away.

2. **Path A — what actually animates** (proven M9/M10, parity-pinned). A
   `RenderNode` carries a `keyframes` block whose `key` is a **scalar** leaf
   path. Solar's `KeyframePlayer` (`@lumencast/runtime` render/keyframe-player.js)
   remounts the framer-motion subtree (`key=` reconciliation) on every delta of
   that leaf → re-plays the *authored, static* keyframe geometry. The only
   authoring vehicle that emits such a block is `wipe-cover`
   (`Orion/internal/compiler/lower_wipe_cover.go`, byte-shape-identical to
   Solar's `buildWipeCoverNode` under a parity oracle). **A leaf delta triggers
   the replay; the leaf never carries geometry** (the A5.5 invariant).

3. **The gap.** `__anim.*` is an **object** → dropped by LSDP §3.2.1
   (scalar-only); AND **no consumer of `__anim.*` exists in Solar /
   `@lumencast/runtime`** (grep: only the Orion exec op + a docstring reference
   it); AND **no compiler lowering turns `animation.play` into a keyframe
   block**. The ADR 003 §3.4 sentence « rendered by Solar/CEF off the delta » is
   **aspirational, not implemented** — Path B writes into a void.

A fourth fact reframes the whole decision, **decisive below**: the runtime has
**two** distinct, proven scalar-leaf animation mechanisms, not one.
- **KeyframePlayer** — replays *authored* keyframe geometry on a scalar-leaf
  trigger (Path A / `wipe-cover`).
- **bindAnimate** (LSML 1.1 §6.3, `render/bind-animate.js`, issue #33/#42) —
  *continuously interpolates* a live scalar leaf toward a framer motion value,
  no remount, per-frame coalesced, R8 filter caps. The live-gauge path.

Both consume **scalar** leaves and are battle-tested on air. `animation.play`
bypasses both by inventing a third (object) channel that nobody renders.

## 2. Decision drivers

- **D1 — Reuse the proven renderer, do not build a second animation engine.**
  Solar doctrine (CLAUDE.md): Solar holds no logic; new visuals are
  Canvas-side compositions, *not* Solar releases. A new `__anim.*` consumer in
  `@lumencast/runtime` is a new runtime primitive — exactly what doctrine and
  Solar's own `wipe-cover.ts` (lines 32-34) refuse.
- **D2 — Scalar-only wire (LSDP §3.2.1).** Whatever triggers the animation must
  reach Solar as a *scalar* leaf. The current object leaf is structurally
  un-renderable.
- **D3 — « Transitions = authored scenes, not code » (durable doctrine).** An
  animation's *geometry* is authored data (a reusable Canvas asset), never wired
  into the compiler. `wipe-cover` already embodies this; `animation.play` must
  not regress to compiled-in geometry.
- **D4 — Expressivity.** `animation.play` carries a **named** `animation_id` +
  dynamic `params` (e.g. `{"score_to": 1891}`); `wipe-cover` carries fixed
  authored timings. The reconciliation must preserve named-asset + param
  injection — this is what makes it more than a `wipe-cover` special case.
- **D5 — Keep the #82/#83/#86 completion machinery.** The park-until-done /
  duration-fallback / external-report contract is correct and security-cleared
  (ADR 003 Amendment 1). The fix must not disturb it.
- **D6 — Single source of truth for keyframe geometry.** The Go↔TS parity oracle
  that pins `wipe-cover` must extend to any new lowering, not fork.

## 3. Decision

**Animation-as-authored-keyframe, via compiler lowering (option (a)).**
`core.animation.play@1` is reconciled by making it **lower, at compile time,
onto the proven Path-A KeyframePlayer mechanism** — driven by a **scalar
generation leaf** the exec op already owns. No new Solar consumer; no second
animation engine. Concretely:

### 3.1 The animation asset is authored data (the geometry source of truth)

An **Animation Asset** is an authored, reusable Canvas artefact (per D3 / the
"transitions are authored scenes" memo) identified by `animation_id`. It
declares: (a) the **target element** it animates (a named layout node id within
the scene/overlay), and (b) its **keyframe geometry** as a parameterised
`keyframes` block — the same shape `wipe-cover` emits (`key`, `duration_ms`,
`easing`, `steps[]`), with `params`-substitutable fields. The asset lives where
overlays/scenes live (ZabCanvas), addressable by id at scene-compile time —
exactly as `wipe-cover` timings are authored on the element, never read from the
live leaf.

> This makes `wipe-cover` the **degenerate case**: a `wipe-cover` element is one
> particular authored Animation Asset (opaque-cover opacity reveal/hold/retract).
> The general `animation.play` path subsumes it (§3.5).

### 3.2 The generation leaf is the scalar trigger (the wire fix)

The exec op's `__anim.<overlay_id>.<generation>` **object** leaf is replaced by a
**scalar** generation leaf:

```
__anim.<overlay_id>            ← uint64 generation counter (SCALAR, passes LSDP §3.2.1)
```

The exec op writes the monotone `generation` (it already computes it,
`exec_anim.go:96`) as a bare number into this leaf. The `animation_id` and
`params` are **NOT** carried on the live leaf (they are authored geometry +
compile-time-resolved overrides, per D3/A5.5) — they are resolved by the
compiler when it lowers the asset (§3.3). The leaf's value-change is the M9
replay trigger; `KeyframePlayer` keyed on `__anim.<overlay_id>` remounts and
replays the lowered geometry on every `animation.play` firing.

### 3.3 The compiler lowers the asset into a keyframe RenderNode

A new lowering (sibling of `lower_wipe_cover.go`) resolves, at push/compile time:
the `animation_id` → the authored Animation Asset → its target element id → a
`keyframes`-bearing `RenderNode` whose `key` is `__anim.<overlay_id>`, whose
geometry is the asset's keyframes with `params` substituted (compile-time, for
the static overrides) . The emitted node is **byte-shape-identical to a Solar
oracle builder** (`buildAnimationNode`, the parity twin of `buildWipeCoverNode`),
so the keyframe geometry has one source of truth (D6). `wipe-cover`'s lowering is
refactored to route through this general path (§3.5).

> **`params` split (resolution criterion R5).** Static overrides knowable at
> compile (e.g. duration, direction, target value of a count-up) are baked into
> the lowered geometry. *Per-firing dynamic* `params` are out of scope for v1:
> the keyframe geometry is fixed per push, the generation leaf only re-triggers
> it. Dynamic per-firing value injection (e.g. animating to a *runtime-computed*
> `score_to`) is a **follow-up** — it needs a second scalar leaf consumed by
> `bindAnimate` (§3.6), and is explicitly deferred (Risk R3).

### 3.4 Target referencing (§2 of the brief)

`animation.play.overlay_id` names the **overlay namespace** of the generation
leaf (`__anim.<overlay_id>`). The **animated element** is named by the
**Animation Asset** (its declared target layout-node id), resolved by the
compiler — NOT passed on the wire. This keeps element-targeting an authoring
concern (D3) and the wire scalar-only (D2). The asset's target id must exist in
the composed scene layout at compile time, else the lowering falls through
(renders nothing — the same stance as a non-conforming `wipe-cover`).

### 3.5 Relation to `wipe-cover`

`animation.play` becomes the **generic trigger**; `wipe-cover` becomes **one
authored asset** expressed in the same lowering. `lower_wipe_cover.go` is
refactored to delegate to the new general `buildAnimationNode` path
(reveal/hold/retract → keyframe steps), preserving its parity oracle and the
M10 magenta-plateau probe byte-for-byte. **No geometry is duplicated**; the
oracle is extended, not forked (D6).

### 3.6 Completion / duration (#82/#83/#86) — preserved verbatim

The park-`completed` + duration-fallback + external-report-race machinery
(`exec_anim.go` `finishAnim`, the timer wheel, the authenticated completion
endpoint, ADR 003 Amendment 1) is **unchanged in mechanism**. The only field
that leaves the live command is `duration_seconds` (it stayed server-side for
the fallback timer — it never needed to be on the leaf). The renderer's external
report (when a reporting client is attached) and the server duration fallback
(broadcast viewers) both resolve `completed` exactly as today; the security
contract (completion scope on the service token, version+token-stamped wake key)
is untouched — **no new Bastion surface** (§5).

## 4. Consequences

- **Orion compiler**: new `lower_animation.go` + `buildAnimationNode` oracle;
  `lower_wipe_cover.go` refactored to delegate. The compiler gains a
  scene-compile-time resolution of `animation_id` → authored asset (a new read
  of the Canvas/overlay asset surface at push).
- **Orion runtime**: `exec_anim.go` changes the leaf it writes from the object
  `__anim.<overlay>.<gen>` to the scalar `__anim.<overlay>` = `gen`. The
  completion path is untouched. `animCommand` struct is removed/slimmed.
- **Solar / `@lumencast/runtime`**: **zero new runtime code** — KeyframePlayer
  already consumes a scalar `keyframes.key`. Only the parity oracle
  `buildAnimationNode` is added (a bundle-fragment builder, like
  `buildWipeCoverNode`), used by the Go↔TS parity test.
- **Blue**: the `core.animation.play@1` seed contract (`stdlib_seeder.py`) is
  **unchanged on the wire** (ports `in`/`then`/`completed`, inputs
  `overlay_id`/`animation_id`/`params`). The `target` block stays
  `prism.canvas`. **One stale fact to correct**: the seed docstring +
  `_CORE_NODES` comment (lines 1160-1204) call this a *"Prism main process fans
  `animation:play` over the scene-server WebSocket"* — that Prism-mediated model
  is **dead** and contradicts ADR 003 (Orion exec op writes the leaf directly).
  The comment must be resynced to the lowering model (Scribe, post-accept).
- **ZabCanvas**: an Animation Asset authoring surface (asset = id + target
  element + parameterised keyframes). v1 may seed a minimal hand-authored asset
  catalogue; full Prism authoring UI is downstream.
- **Harness (#78/#53)**: the existing animation harness rides `wipe-cover` for a
  *visible* result. Post-reconciliation it can drive a real `animation.play`
  asset and assert frame-diff movement (R5). Until the asset surface lands, the
  harness keeps `wipe-cover` (now a delegated asset) — no regression.

## 5. Risks

- **R1 — Asset resolution at compile is a new Canvas read (MED).** The compiler
  must resolve `animation_id` against an authored asset catalogue at push time.
  Where that catalogue lives (inlined in the Canvas layout vs a ZabCanvas asset
  endpoint) is an **inter-repo contract → Conduit** (issue below). Mitigation:
  v1 inlines the asset in the scene layout (no cross-service fetch), matching
  how `wipe-cover` props are already authored inline.
- **R2 — Object→scalar leaf is a wire-contract change.** `__anim.<overlay>.<gen>`
  (object, currently filtered) → `__anim.<overlay>` (scalar). No consumer exists
  today, so nothing breaks; but it is an LSDP-visible leaf shape change — Conduit
  records it in the LSDP/render-bundle contract. Amends ADR 003 §3.1.3's command
  shape (this ADR is the amendment record).
- **R3 — Dynamic per-firing `params` deferred (LOW, scoped out).** v1 animates
  fixed (compile-baked) geometry re-triggered by the generation leaf. Animating
  toward a *runtime-computed* value needs a second scalar leaf on `bindAnimate`
  (§3.6) — a clean follow-up, not a v1 blocker. Called out so it is not mistaken
  for a gap.
- **R4 — Security: none new.** Rendering is local (CEF). The completion contract
  is unchanged (ADR 003 Amendment 1, already Bastion-cleared). The scalar leaf is
  a generation counter (no PII, no injection vector). **No Bastion spawn required
  for this ADR.** If the Conduit asset-catalogue contract (R1) adds a
  cross-service fetch with auth, *that* PR re-enters Bastion clearance — flagged,
  not assumed.

## 6. Resolution criteria (testable)

1. **Observable movement on air (the criterion that was missing).** A blueprint
   firing `core.animation.play@1` against an authored Animation Asset produces a
   **frame-diff-detectable movement** in a real `.mp4` capture (per
   `live-testing.md`: `ffmpeg` spatial-stddev / luma delta across the animation
   window). A snapshot/wire-only proof is insufficient.
2. **Scalar leaf on the wire.** The LSDP keyframe-on-join + delta carries
   `__anim.<overlay_id>` as a **scalar** uint64; it is no longer filtered by
   §3.2.1. (Orion runtime test + LSDP fixture.)
3. **Parity preserved.** The Go↔TS parity test asserts `lower_animation.go`'s
   emitted node == Solar's `buildAnimationNode` (decoded values); the existing
   `wipe-cover` parity + M10 magenta-plateau probe still pass byte-for-byte after
   the delegation refactor.
4. **Completion unchanged.** `then` fires immediately; `completed` resolves via
   external report OR duration fallback exactly as the ADR 003 Amendment 1 tests
   assert (no test in that suite changes behaviour).
5. **Replay on re-fire.** Two successive `animation.play` firings bump the
   generation leaf twice → KeyframePlayer remounts twice → the sequence replays
   twice (unit test on the KeyframePlayer trigger; frame-diff on air shows two
   movements).
6. **No new Solar runtime primitive.** `@lumencast/runtime` gains no
   `__anim`-consuming code; the diff is the oracle builder + the parity test
   only (Solar bundle-size budget unchanged).

---

## 7. Alternatives considered (rejected)

- **(b) New Solar consumer of `__anim.*`** — add code in `@lumencast/runtime`
  that, on a generation-leaf delta, plays the named animation (framer-motion) on
  the target element. **Rejected**: (i) it is a *new runtime primitive*, which
  Solar doctrine forbids (logic lives in the graph artefact, not in Solar;
  `wipe-cover.ts:32-34` explicitly refuses this); (ii) it builds a *second*
  animation engine alongside KeyframePlayer + bindAnimate, splitting the proven
  path; (iii) the animation geometry would have to travel the wire or be
  hard-known by Solar — either re-introduces object leaves (D2) or compiled-in
  geometry (D3). Option (a) reuses the exact M9/M10-proven mechanism with zero
  new render code.
- **(c) bindAnimate (continuous interpolation) as the primary path** — drive a
  scalar value leaf and let `bindAnimate` interpolate toward it. **Rejected as
  the *primary* model**: `bindAnimate` interpolates toward a *live target* (a
  gauge filling), it does not *replay a named, multi-phase sequence*
  (reveal/hold/retract, count-ups with easing). It is the right tool for the
  *dynamic-value* follow-up (R3/§3.6), not for the named-asset replay
  `animation.play` is about. Kept in reserve for the deferred dynamic-`params`
  work, not adopted now.
- **Keep object `__anim.*` + lift the LSDP scalar-only rule** — **Rejected**:
  §3.2.1 scalar-only is a load-bearing wire invariant (keeps the delta pipe and
  the signal store leaf-grain); punching an object through it for one op is a
  doctrine regression with no upside over the scalar generation leaf.

## 8. Issue decomposition

| # | Title | Repo | Scope | Depends on | Conduit? |
|---|---|---|---|---|---|
| I1 | Animation Asset authoring shape (id + target element + parameterised keyframes), inlined in scene layout | ZabCanvas | Define + persist the authored asset shape; v1 inline-in-layout (no cross-service fetch) | — | — |
| I2 | `buildAnimationNode` oracle + `lower_animation.go` (resolve `animation_id`→asset→keyframe RenderNode keyed on `__anim.<overlay>`) | Orion (compiler) + Solar (oracle twin) | The lowering + the Go↔TS parity test; static `params` baked | I1 | — |
| I3 | `exec_anim.go`: write scalar generation leaf `__anim.<overlay>` instead of object; drop `animCommand` | Orion (runtime) | Wire fix; completion path untouched | — | — |
| I4 | Refactor `lower_wipe_cover.go` to delegate to `buildAnimationNode`; keep M10 probe + parity green | Orion + Solar | No geometry duplication | I2 | — |
| I5 | LSDP / render-bundle contract: `__anim.<overlay>` scalar leaf shape; record the object→scalar amendment | Orion (+ contract doc) | Inter-repo wire shape | I3 | **Yes** |
| I6 | Resync Blue seed comment (kill the dead "Prism fans animation:play over WS" model; document lowering model) | Blue | Doc-only; no wire change | — | — |
| I7 | Live frame-diff proof + harness (#78/#53) drives a real `animation.play` asset | Orion/test harness | R5 criterion #1 | I1–I4 | — |

> **Chain.** `/build` per issue (Forge→Probe), with **Conduit** on I5 (LSDP +
> render-bundle wire shape across Orion/Solar). **No Bastion** unless I1's asset
> catalogue grows a cross-service authenticated fetch (R4). Vigil flips this ADR
> `proposed→accepted` on the maintainer's go.

## 9. Effort estimate

Medium. The renderer (the hard, proven part) is **reused untouched** — the work
is a compiler lowering + a small runtime leaf-shape change + an oracle test, all
modelled on the existing `wipe-cover` pair. The genuinely new surface is the
Animation Asset authoring shape (I1) and its compile-time resolution. ~5-7
issues, 1-2 of them (I1, I2) non-trivial; the rest are mechanical or doc.
