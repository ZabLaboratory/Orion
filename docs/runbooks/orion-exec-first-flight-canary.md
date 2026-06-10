# Runbook — Exec first-flight canary (R9-lift, ADR 006 §3.6)

> ADR 006 (accepted, `1ae92e2`) §3.6 — *operational first flight*. Companion to
> the CPU-isolation runbook `orion-cpu-pathological-scene.md` (#89). Owner:
> Keeper. Surface: VPS `vps-ovh` (`ubuntu@51.91.126.43`), app path
> `/home/ubuntu/orion`, container `orion` on `zab-internal`, gateway-only via
> ZabGate at `https://zabgate.cyell.dev/orion`.

## What this is

The procedure for the **first ever exec-bearing scene to reach the antenna**
once the R9 lift (ADR 006 §3.4, future #106) is merged and deployed. Until #106
ships, exec is dormant and this runbook does not apply. It describes the
**controlled window**, the **metrics to watch**, and the **single incident
lever** — a validated rollback, never a process kill.

## Doctrine (do not violate)

- **No-kill.** Exactly as #89: a scene is never killed, skipped, or amputated
  (ADR 003 §1.1). The incident lever here is the **validated-rollback / authored
  active-scene switch** (gate #87, ADR 003 criterion #16), the same authored
  drain used in #89 §3. There is **no `docker kill`, no `docker restart` as a
  remedy**, no feature flag (ADR 006 §3.6 forbade one — the gate IS the rollout
  control).
- **Gate is the only behaviour control.** The canary airs because its
  `scene_version` carries a `validated` record for the current `harness_version`
  (ADR 006 §3.4 invariant). Nothing reaches air unvalidated.

## 0. Preconditions (must ALL hold before opening the window)

| # | Precondition | Check |
|---|---|---|
| 1 | R9-lift (#106) merged, CI + deploy green on `main` | `gh run list -R ZabLaboratory/Orion -w deploy.yml -L 1` green |
| 2 | Prerequisites P-1…P-5 (ADR 006 §3.7) closed or held-by-policy | links on the #106 PR |
| 3 | CPU cap live = 2.0 (#108 / P-3) | `ssh vps-ovh "docker inspect orion -f '{{.HostConfig.NanoCpus}}'"` → `2000000000` |
| 4 | Worker pool coherent with cap | `ORION_EFFECT_WORKERS=4` in étage-1 `.env` (see #89 §Sizing) |
| 5 | Health + readiness green | `GET /orion/api/v1/health` = 200, `GET /orion/api/v1/ready` = 200 |
| 6 | Canary scene authored + pushed + **validated** | `GET /orion/api/v1/scenes/<canary-id>` shows latest version `validated` |
| 7 | A known-good rollback scene is on air (the current live scene) | note its `scene_id` + version below — this is the incident target |

> **Record before the window** (paste into the incident log):
> - `CANARY_ID` / `CANARY_VERSION` = ____
> - `ROLLBACK_ID` / `ROLLBACK_VERSION` (the known-good scene to switch back to) = ____
> - Window operator (human, on-console) = ____
> - Start / planned-end time = ____

## 1. The canary scene (maintainer-authored)

Per ADR 006 §3.6 the canary **exercises the exec layer end-to-end without being
an antenna-critical show element**: it must include
- a **loop** (`for-loop` / `for-each` / `while`),
- **variables** (`variable.set` / `variable.get`),
- a **delay** (`delay` — timer-wheel path),
- **one async effect** (a single `http.request` *or* `db.query` *or*
  `source.read` — the cheapest real-world op; not all of them at once).

It is authored by the maintainer, pushed, and validated through the gate (#87)
**before** the window. Keep it minimal — its job is to light up every exec
subsystem under observation, not to be a flashy scene.

## 2. Open the window — air the canary

The window is **short, attended, and reversible**. Air the canary via the
authored active-scene switch (same API as the #89 drain, in reverse):

```bash
# switch the live show to the validated canary:
ssh vps-ovh "curl -fsS -X POST https://zabgate.cyell.dev/orion/api/v1/show/active-scene \
  -H 'content-type: application/json' -d '{\"scene_id\":\"<CANARY_ID>\"}'"
# expect 200 + a scene_changed/snapshot on /show/stream subscribers.
# If this returns SCENE_NOT_VALIDATED, the gate refused — STOP, the canary is
# not validated; do not force anything (precondition 6 failed).
```

Confirm it is live:

```bash
ssh vps-ovh "curl -fsS https://zabgate.cyell.dev/orion/api/v1/show"   # active scene == CANARY_ID
```

## 3. Watch — the §3.2.2 metrics (the whole point of the window)

Metrics are on Orion's **internal-only** endpoint `/internal/metrics`
(`ORION_INTERNAL_ADDR`, default `0.0.0.0:4017` — `expose`d on `zab-internal`,
never gateway-routed). No alerting stack lives in-repo yet, so watch them by
hand from the box for the window's duration. Scrape from inside the network:

```bash
# from the VPS, hit the internal metrics endpoint on the container:
ssh vps-ovh "docker run --rm --network zab-internal curlimages/curl -fsS \
  http://orion:4017/internal/metrics \
  | grep -E 'orion_task_cpu_seconds_total|orion_task_preempt_total|orion_task_budget_exceeded_total|orion_event_shed_total|orion_parked_tasks|orion_timer_wheel_size'"
```

| Metric | Type | Healthy canary | Watch-for (incident signal) |
|---|---|---|---|
| `orion_task_cpu_seconds_total{scene_id,scene_version}` | counter | rises gently, well under cap | `rate(...[5m]) > 0.8` sustained 10 min on the canary version → see #89 §2 — scene nearing its cgroup share |
| `orion_task_preempt_total` | counter | a few preempts (time-slicing working) | a *runaway* count climbing without bound = a task that won't terminate within budget repeatedly re-sliced |
| `orion_task_budget_exceeded_total` | counter | **0**, or rare | climbing = a task systematically over its per-task budget (the B7 signal — validated-but-costly, ADR 006 R-1) |
| `orion_event_shed_total` | counter | **0** ideally | climbing = events firing faster than the budget drains them (e.g. on-event re-fired per chat msg, ADR 006 R-1) — running tasks untouched, but the scene is overloaded |
| `orion_parked_tasks` | gauge | low, returns to ~0 (delays/awaits resolve) | stuck high / monotonically rising = continuations parked and never resumed (delay/effect not completing) |
| `orion_timer_wheel_size` | gauge | small, bounded | unbounded growth = timers armed faster than they fire (delay leak) |

Also keep the **host + cgroup** view from #89 §4 open in parallel — the cap must
be holding:

```bash
ssh vps-ovh "docker stats --no-stream orion"   # CPU% must stay well below 200% (2-core cap); host healthy
ssh vps-ovh "uptime"                            # host load not climbing toward saturation
```

**Healthy first flight** = canary airs, all exec subsystems show activity
(`preempt`/`cpu_seconds` move, `parked`/`timer_wheel` rise-and-fall cleanly),
`budget_exceeded` and `event_shed` stay at 0, host CPU% nowhere near the 2-core
cap, `/health` + `/ready` stay 200, `/show/stream` subscribers keep receiving
deltas.

## 4. Incident lever — validated rollback (the ONLY remedy)

If any watch-for signal fires (sustained CPU burn, `budget_exceeded` /
`event_shed` climbing, parked tasks stuck, timer wheel unbounded, or the show
visibly degrading), the response is **switch the air back to the known-good
scene** — authored, gate-approved, no kill:

```bash
# 1. drain the canary the authored way — switch active scene to the known-good:
ssh vps-ovh "curl -fsS -X POST https://zabgate.cyell.dev/orion/api/v1/show/active-scene \
  -H 'content-type: application/json' -d '{\"scene_id\":\"<ROLLBACK_ID>\"}'"
# switch-away cancels the canary's exec tasks the authored way (ADR 003 §3.1.4
# cancellation-on-switch) — the runaway drains without a process kill.

# 2. (optional, if the canary version is genuinely pathological) archive it so
#    it cannot be re-aired by accident:
ssh vps-ovh "curl -fsS -X POST https://zabgate.cyell.dev/orion/api/v1/scenes/<CANARY_ID>/status \
  -H 'content-type: application/json' -d '{\"status\":\"archived\"}'"
```

Confirm recovery:

```bash
ssh vps-ovh "curl -fsS https://zabgate.cyell.dev/orion/api/v1/show"   # active scene == ROLLBACK_ID
ssh vps-ovh "curl -fsS -o /dev/null -w 'health=%{http_code}\n' https://zabgate.cyell.dev/orion/api/v1/health"  # 200
```

**Do NOT** `docker kill` / `docker restart` `orion` as the remedy — it kills the
whole show (every scene, every viewer WS), masks the cause, and violates
doctrine (identical to #89 §3 step 4). The cgroup cap (#89 / #108) is the host's
protection meanwhile; the validated switch is the show's.

## 5. After the window

- **Healthy flight**: record the metric maxima observed; the canary version is
  the reference baseline for tuning the #89 alert thresholds against *observed*
  normal (the #89 §2 PromQL `0.8` is a placeholder until exec has run live).
- **Aborted flight**: capture the offending scene-version hash + the inputs/
  conditions that triggered it — it is a candidate to harden the validation gate
  (#87) so the next push of that pattern is caught at authoring, not on air
  (ADR 006 R-1; #89 §3 step 5).
- Either way: a short incident note (Scribe-formattable) with the recorded
  fields from §0, the metric maxima, and the outcome.

## 6. Rollback of the lift itself (deploy-level, distinct from the in-air lever)

The in-air lever (§4) handles a bad *scene*. If the **lift code** itself
misbehaves on air (a partition/install regression, not a scene), the deploy-
level rollback is the standard Orion deploy revert — revert the #106 merge on
`main`, the next `deploy.yml` run rebuilds and `up -d --force-recreate orion`
restores the prior image. No volume, no DB, no env touched (pure code +
compose). Exec returns to dormant (no validated exec scene installed). Document
in the incident note.

## Quick reference

| Check | Command | Healthy |
|---|---|---|
| Canary live | `GET /orion/api/v1/show` | active == `CANARY_ID` |
| CPU cap holding | `docker stats --no-stream orion` | CPU% « 200%, host OK |
| Budget breaches | `orion_task_budget_exceeded_total` | 0 / rare |
| Events shed | `orion_event_shed_total` | 0 |
| Parked tasks | `orion_parked_tasks` | low, returns to ~0 |
| Timer wheel | `orion_timer_wheel_size` | small, bounded |
| Incident lever | `POST /show/active-scene {scene_id: ROLLBACK_ID}` | active == `ROLLBACK_ID`, health 200 |
