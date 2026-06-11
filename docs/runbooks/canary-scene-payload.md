# Runbook — Canary first-flight scene: payload + commands (R9, issue #106)

> Companion to `orion-exec-first-flight-canary.md` (the operational window /
> metrics / rollback procedure). **That** runbook leaves the canary scene itself
> as a precondition (§0 row 6, §1: *maintainer-authored*). **This** file fills
> that gap: the concrete **pushable payload**, the **push → validate → activate**
> command sequence Keeper executes, and the **exact observable leaves/metrics**
> that prove the exec ran LIVE. Owner: Keeper executes; Forge produced.
> Surface: VPS `vps-ovh`, gateway-only via ZabGate `https://zabgate.cyell.dev/orion`.

## 0. What this proves (the franchissement)

The first exec-bearing scene to reach the antenna. On activation, behind the
validation gate (#87), Orion installs the canary's `ExecProgram` and fires
`on-start` live. The scene runs the **maintainer's loop** (ADR 003 #4 / criterion
#5) plus minimal observability instrumentation, all **LIVE ops only**.

### ⚠️ HARD CONDITION (Bastion first-flight) — LIVE ops ONLY

The canary binds **only** these compute families: `core.event.on-start@1`,
`core.event.on-tick@1`, `core.literal@1`, `core.flow.for-loop@1`,
`core.variable.set@1`, `core.variable.get@1`, `core.print@1`, `core.math.add@1`.

**No `http.request`, no `db.query`, no `source.read`.** Those halt-at-node
silently under the SetEffects hold (deny-all egress / no DataSource on this box)
— a muddied flight. This invariant is asserted in CI two ways:
`tests/e2e/canary_firstflight_test.go::TestE2E_Canary_UsesOnlyLiveOps` (the op
set is a subset of a closed allow-list) and by construction in the harness
`internal/runtime/canary_firstflight_harness_test.go`.

## 1. The canary logic (what it exercises)

```
on-start ─then→ for-loop(first=0, last=9)
                  ├─ body ─exec_in→ variable.set(counter = index)        [data: index→value]
                  └─ completed ─exec_in→ print("canary first-flight: …")  [data: literal→message]

on-tick  ─then→ variable.set(ticks = ticks + 1)   [air-only trigger]
                  value ← math.add(a = variable.get(ticks), b = 1)
```

| Subsystem exercised | Node(s) | What it proves |
|---|---|---|
| trigger (on-start) | `start` → `loop` | exec installs + fires when the scene goes on air |
| **loop** (bounded) | `for-loop(0..9)` | the scheduler runs a bounded loop, time-sliced, no-kill |
| **variable state** | `variable.set(counter)` | exec produces observable state → `__vars..counter` climbs to `9` |
| **print / `__debug` ring** | `print` on `completed` | a line lands in `__debug..print` (observable in the live snapshot) |
| **air-only trigger + tick wheel** | `on-tick` → `variable.set(ticks)` | `on-tick` is `triggersGated` (fires ONLY on air) → `__vars..ticks` climbing proves the global tick fires the exec live, not backstage |
| **variable.get + compute** | `variable.get(ticks)` → `math.add` | read-modify-write of a live variable each tick |

> **Why no `delay` node here.** A `delay` would also be a clean live op (timer
> wheel / park-resume), but the canary stays minimal and **terminating**:
> `on-start` runs once and ends; `on-tick` is a tiny bounded read-modify-write per
> tick. The `delay`/park-resume path is observable anyway via `orion_parked_tasks`
> /`orion_timer_wheel_size` if the maintainer chooses to add one — the allow-list
> in the e2e guard already permits `core.flow.delay@1` and `core.animation.play@1`.

The blueprint **graph** (the `bp-canary` Blue artefact Orion's compiler fetches)
is pinned verbatim, with typed exec/data pins, in:
- `internal/runtime/canary_firstflight_harness_test.go::canaryHarnessBlueprint` (DB-free)
- `tests/e2e/canary_firstflight_test.go::canaryBlueprint` (full e2e)

These are the source of truth for the graph; reproduce it in Blue under id
`bp-canary` (see §2 prerequisite).

## 2. Prerequisite — author `bp-canary` in Blue (HYPOTHESIS to confirm)

The push envelope (`canary-scene-payload.json`) carries `blue_blueprint_id:
"bp-canary"`. Orion's compiler **fetches that blueprint from Blue** at push time
(`FetchBlueprint`); it is NOT inlined in the envelope. **Therefore `bp-canary`
must exist in Blue, resolving to the graph in §1, before the push.** Two options:

- **(a)** the maintainer authors `bp-canary` in Blue (Prism/Canvas) matching the
  pinned graph, OR
- **(b)** if directly seeding Blue's store is out of scope, the canary is pushed
  against a test Canvas/Blue stub exactly as the e2e test does — but **on prod the
  real Blue must serve it**.

This is a real cross-service dependency (Orion ← Blue). **Flagged to Eleven** as
an assumption: this artefact assumes `bp-canary` is authored in Blue; confirm
with the Blue owner / maintainer before the window. Everything Orion-side
(compile → exec emit → validate → air) is proven; the Blue authoring step is the
one human prerequisite.

## 3. The push payload

`canary-scene-payload.json` (next to this file):

```json
{ "canvas_version": "v1", "blue_blueprint_id": "bp-canary" }
```

- **scene_id**: Orion scene ids are **UUIDs** (`parseUUID` on every scene route).
  `canary-r9-firstflight` is the human label, **not** the path id. Pick/record a
  fixed UUID as `CANARY_ID` (e.g. `uuidgen`) and use it on every command below.
  The scene name (Canvas's, §3.2 ADR 002) can be `canary-r9-firstflight`.
- The envelope is the **legacy single-blueprint** shape → the compiler assigns
  the empty blueprint key `""`, so leaf paths are `__vars..counter` (double dot),
  `__vars..ticks`, `__debug..print`. **Verified** by the harness key-probe.

## 4. Command sequence (templates — Keeper fills `$OP_TOKEN`, `CANARY_ID`)

All calls are **gateway-routed** through ZabGate and **authenticated** (operator
or admin role, injected by ZabGate from the bearer token). `push`/`validate`/
`active-scene` all require operator/admin (`requireOperator`). Never paste the
token into this file or the incident log.

```bash
export CANARY_ID="<uuid>"            # the recorded canary scene UUID
# export OP_TOKEN in the operator shell (same token used to push/validate)

# 4.1 PUSH the exec-bearing canary (compiles bp-canary from Blue, persists).
ssh vps-ovh "curl -fsS -X POST \
  https://zabgate.cyell.dev/orion/api/v1/scenes/$CANARY_ID/push \
  -H 'authorization: Bearer \$OP_TOKEN' -H 'content-type: application/json' \
  -d '{\"canvas_version\":\"v1\",\"blue_blueprint_id\":\"bp-canary\"}'"
# expect 200 + {"scene_version":"sha256:…","diagnostics":{"errors":[],"warnings":[]}}.
# RECORD CANARY_VERSION = the returned scene_version.
# 422 COMPILE_FAILED = bp-canary missing/malformed in Blue (see §2).

# 4.2 VALIDATE the latest pushed version (async campaign; returns 202 running).
ssh vps-ovh "curl -fsS -X POST \
  https://zabgate.cyell.dev/orion/api/v1/scenes/$CANARY_ID/validate \
  -H 'authorization: Bearer \$OP_TOKEN' -H 'content-type: application/json' -d '{}'"
# expect 202 + {"scene_version":"sha256:…","harness_version":"…","status":"running"}.

# 4.3 POLL validation until status == validated.
export CANARY_VERSION="<sha256:… from 4.1>"
ssh vps-ovh "curl -fsS \
  'https://zabgate.cyell.dev/orion/api/v1/scenes/$CANARY_ID/validation?v=$CANARY_VERSION' \
  -H 'authorization: Bearer \$OP_TOKEN'"
# repeat until 200 + {"status":"validated", …}. Anything else → STOP (gate held).

# 4.4 ACTIVATE — THE FRANCHISSEMENT. Switches the live show to the canary.
ssh vps-ovh "curl -fsS -X POST \
  https://zabgate.cyell.dev/orion/api/v1/show/active-scene \
  -H 'authorization: Bearer \$OP_TOKEN' -H 'content-type: application/json' \
  -d '{\"scene_id\":\"$CANARY_ID\"}'"
# expect 200. on activation Orion installs the ExecProgram and fires on-start.
# 409 SCENE_NOT_VALIDATED = the gate refused (4.3 not validated). Do NOT force.
```

> **Order matters.** Activate-before-validate is **refused** with `409
> SCENE_NOT_VALIDATED` (the #87 gate holds for exec too) — asserted in both
> canary tests. Validate first, confirm `validated`, then activate.

## 5. Success criteria — observable proof the exec ran LIVE

After 4.4, observe **both** the live snapshot leaves (via a `/show/stream`
subscriber, e.g. Solar/Prism, or the snapshot the switch emits) **and** the
internal metrics. The exec ran live iff:

| Signal | Where | Healthy value |
|---|---|---|
| **`__vars..counter`** (note the double dot) | live snapshot `state` | **`9`** — the maintainer's loop completed; the criterion #5 proof |
| **`__debug..print`** | live snapshot `state` | a non-empty JSON ring containing `"canary first-flight: maintainers loop complete (counter=9)"` |
| **`__vars..ticks`** | live snapshot `state` | **climbing past 0** — `on-tick` fires ON AIR (air-only trigger + global tick wheel live). Stuck at `0`/absent = exec not firing on tick |
| `orion_task_cpu_seconds_total{scene_id=CANARY_ID}` | `/orion/internal/metrics` | series **appears** and rises gently (a metric series is not emitted until that scene runs an exec task — its appearance alone proves exec ran) |
| `orion_task_preempt_total` | `/orion/internal/metrics` | a few preempts (time-slicing working) |
| `orion_parked_tasks`, `orion_timer_wheel_size` | `/orion/internal/metrics` | low / bounded, breathe back toward 0 |
| `orion_exec_completion_rejected_total`, `orion_exec_resume_stale_total` | `/orion/internal/metrics` | **0** steady-state |

The **single load-bearing proof** is `__vars..counter == 9` on the live snapshot
combined with the `orion_task_*` series existing for `CANARY_ID`: state was
produced by exec running on the antenna. `__vars..ticks` climbing is the
secondary proof that air-only triggers fire live.

See `orion-exec-first-flight-canary.md` §3–§4 for the full metric watch table and
the **incident lever** (validated rollback / `POST /show/active-scene
{ROLLBACK_ID}` — no kill, no restart).

## 6. Pre-flight proof (already green, no antenna touched)

Forge proved the payload is well-formed **before** any push:

- `internal/runtime/canary_firstflight_harness_test.go::TestCanary_CompilesEmitsExecAndRunsLive`
  (DB-free): drives the **real compiler** on the canary blueprint → asserts it
  COMPILES (no error diagnostics), EMITS an `ExecProgram` with **both** `on-start`
  and `on-tick` entrypoints, and RUNS LIVE in a real `Scene` → `__vars..counter
  == 9` and the print lands in `__debug..print`. **PASS locally.**
- `tests/e2e/canary_firstflight_test.go` (CI, live PG): flies the EXACT payload
  through the real `push → (refuse activate) → validate → activate` API and
  asserts counter==9, the print ring, and `ticks` climbing on air. Plus
  `TestE2E_Canary_UsesOnlyLiveOps` (the Bastion live-ops allow-list guard, runs
  without a DB).
