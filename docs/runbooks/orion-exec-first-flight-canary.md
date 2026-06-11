# Runbook — Exec first-flight canary (R9-lift, ADR 006 §3.6)

> ADR 006 (accepted, `1ae92e2`) §3.6 — *operational first flight*. Companion to
> the CPU-isolation runbook `orion-cpu-pathological-scene.md` (#89). Owner:
> Keeper. Surface: VPS `vps-ovh` (`ubuntu@51.91.126.43`), app path
> `/home/ubuntu/orion`, container `orion` on `zab-internal`, gateway-only via
> ZabGate at `https://zabgate.cyell.dev/orion`.

## What this is

The procedure for the **first ever exec-bearing scene to reach the antenna**.
The R9 lift (ADR 006 §3.4, #106) is **merged + deployed** (`c1358c6`,
2026-06-11): the exec→antenna capability is **armed** behind the validation gate
(#87), but no exec scene is validated/active yet, so exec is **dormant in
practice** until the maintainer authors + validates + airs the canary. This
runbook now applies. It describes the **controlled window**, the **metrics to
watch**, and the **single incident lever** — a validated rollback, never a
process kill.

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
| 4 | Worker pool coherent with cap | **APPLIED 2026-06-11**: `ORION_EFFECT_WORKERS=4` set in étage-1 `.env` on vps-ovh + Orion recreated, boot healthy. Verify: `ssh vps-ovh "docker inspect orion -f '{{range .Config.Env}}{{println .}}{{end}}'" \| grep ORION_EFFECT_WORKERS` → `4`. (Not exercised by a live-ops-only canary — §1 — but the coherent pre-lift state.) |
| 5 | Health + readiness green | `GET /orion/api/v1/health` = 200, `GET /orion/api/v1/ready` = 200 (both **unauthenticated**) |
| 6 | Canary scene authored + pushed + **validated** | `GET /orion/api/v1/scenes/<canary-id>` (auth: `-H "authorization: Bearer $OP_TOKEN"`) shows latest version `validated`. **And it contains NO `http.request`/`db.query`/`source.read`** — §1 hard condition. |
| 7 | A known-good rollback scene is on air (the current live scene) | note its `scene_id` + version below — this is the incident target |

> **Record before the window** (paste into the incident log):
> - `CANARY_ID` / `CANARY_VERSION` = ____
> - `ROLLBACK_ID` / `ROLLBACK_VERSION` (the known-good scene to switch back to) = ____
> - Window operator (human, on-console) = ____
> - Start / planned-end time = ____

## 1. The canary scene (maintainer-authored)

Per ADR 006 §3.6 the canary **exercises the exec layer end-to-end without being
an antenna-critical show element**. It must light up the exec scheduler — the
**live ops only**:
- a **loop** (`for` / `for-each` / `while`),
- **variables** (`variable.set` / `variable.get`),
- a **delay** (`delay` — timer-wheel + park/resume path),
- and any pure live op: `branch` / `sequence` / `gate` / `compute` / `print`,
- optionally an **`animation.play`** (live animation effect).

> ### ⚠️ HARD CONDITION (Bastion first-flight) — live ops ONLY
>
> The canary scene **MUST NOT** contain a single **async / impure effect**:
> **no `http.request`, no `db.query`, no `source.read`.** First flight exercises
> the scheduler (slicing, parking, timer wheel, cancellation) with deterministic
> live ops *only*.
>
> Reason: the async-effect surface fails **silently** on this box. Egress is
> deny-all by default (`ORION_HTTP_EGRESS_ALLOW_HOSTS=` empty) and no DataSource
> is declared (`ORION_DATASOURCES=` empty). A `http.request` / `db.query` /
> `source.read` therefore **halts at that node** — the continuation parks and
> never resumes (the same *SetEffects-hold* failure mode), `orion_parked_tasks`
> climbs and sticks, and the canary looks "stuck" with no clean signal. That is
> a **muddied** first flight, not a clean one.
>
> The async-effect path (and its worker pool — see §0 P-3) gets its own,
> separate, deliberately-wired flight **later**, once egress hosts / DataSources
> are declared and validated. The canary does **not** test it.

It is authored by the maintainer, pushed, and validated through the gate (#87)
**before** the window. Keep it minimal — its job is to light up the **live** exec
subsystems under observation, not to be a flashy scene and not to touch I/O.

### 1.1 How exec reaches air (#106 wiring — single seam)

Post-#106 (`c1358c6`), exec install is wired through **one** seam, `execForAir`
(`internal/api/gate.go`), used by **every** production activation path. There is
no feature flag — **the validation record IS the rollout control**:

| # | Activation path | Handler |
|---|---|---|
| 1 | push-swap of an active scene | `pushScene` (`scenes_push.go`) — validated → swap **with exec**; not-validated → antenna keeps the last validated version, returns `SCENE_NOT_VALIDATED` |
| 2 | active-scene switch | `POST /show/active-scene` (`show.go`) — §2 below |
| 3 | validation success (reload) | `POST /scenes/{id}/validate` (`scenes_validate.go`) |
| 4 | rollback | `handleRollback` (`scenes_push.go`) |
| — | boot reseed | `ExecForBoot` (`gate.go`) — same gate composition at cold start |

**The "air-only" / eligibility flag.** `execForAir` returns `(progs, eligible)`.
`eligible` is the antenna-move decision, **distinct** from the program set: a
**validated pure-dataflow** scene is `eligible` (it swaps onto air) yet carries
**zero exec programs**. Exec programs are installed **only** for a validated
*exec-bearing* version. Postures: DB error → fail-closed (refuse, install
nothing); validated-but-corrupt artefact → fail-loud; unproven version →
dataflow-only, no exec, **authoring never blocked**. The canary moves onto air
**iff** its `scene_version` carries a `validated` record for the current
`harness_version` — pushing/airing an unvalidated version returns
`SCENE_NOT_VALIDATED` and the antenna does not move.

## 2. Open the window — air the canary

The window is **short, attended, and reversible**. Air the canary via the
authored active-scene switch (same API as the #89 drain, in reverse).

> **Auth.** These are **gateway-routed** calls through ZabGate. The `/show` read
> and the `/show/active-scene` write are **authenticated** — a bare `curl`
> without a token returns **401** (verified 2026-06-11). Pass the operator's
> bearer token: `-H "authorization: Bearer $OP_TOKEN"`. Export `OP_TOKEN` in the
> on-console operator's shell for the window; it is the same token the maintainer
> already uses to `push`/`validate` a scene. Never paste the token into this
> runbook or the incident log.

```bash
# switch the live show to the validated canary:
ssh vps-ovh "curl -fsS -X POST https://zabgate.cyell.dev/orion/api/v1/show/active-scene \
  -H 'authorization: Bearer $OP_TOKEN' -H 'content-type: application/json' \
  -d '{\"scene_id\":\"<CANARY_ID>\"}'"
# expect 200 + a scene_changed/snapshot on /show/stream subscribers.
# If this returns SCENE_NOT_VALIDATED, the gate refused — STOP, the canary is
# not validated; do not force anything (precondition 6 failed).
# 401 = missing/expired token (not a scene problem); re-issue OP_TOKEN.
```

Confirm it is live:

```bash
ssh vps-ovh "curl -fsS -H 'authorization: Bearer $OP_TOKEN' \
  https://zabgate.cyell.dev/orion/api/v1/show"   # active scene == CANARY_ID
```

## 3. Watch — the §3.2.2 metrics (the whole point of the window)

Metrics are on Orion's **internal-only** endpoint — the real route is
**`/orion/internal/metrics`** (`ORION_INTERNAL_ADDR`, default `0.0.0.0:4017` —
`expose`d on `zab-internal`, never gateway-routed). Note the `/orion` prefix:
the bare `/internal/metrics` returns **404** (verified 2026-06-11). No alerting
stack lives in-repo yet, so watch them by hand from the box for the window's
duration. Scrape from inside the network:

```bash
# from the VPS, hit the internal metrics endpoint on the container
# (the curlimages/curl image is already pulled on vps-ovh):
ssh vps-ovh "docker run --rm --network zab-internal curlimages/curl:latest -fsS \
  http://orion:4017/orion/internal/metrics \
  | grep -E 'orion_task_cpu_seconds_total|orion_task_preempt_total|orion_event_shed_total|orion_parked_tasks|orion_timer_wheel_size|orion_exec_completion_rejected_total|orion_exec_resume_stale_total'"
```

> **Empty before the canary fires is NORMAL.** Every exec metric above is a
> Prometheus `*Vec` labelled by `scene_id` — a series is **not emitted until that
> scene has actually run an exec task**. Before the canary airs, the scrape
> returns the endpoint fine (HTTP 200) but the `grep` matches **nothing**. Once
> the canary fires its first node, the series appear. Verify the endpoint itself
> with `-o /dev/null -w '%{http_code}'` (expect `200`) if in doubt — an empty
> grep is not a broken scrape.
>
> **`orion_task_budget_exceeded_total` does NOT exist** as a separate series.
> The B5 per-scene task-budget back-pressure (queued + parked ≥ budget) sheds the
> *new* fire and counts it on **`orion_event_shed_total`** — the same counter as
> general event-shedding. Watch `orion_event_shed_total`; do not grep for a
> `budget_exceeded` series (it returns nothing, always).

| Metric | Type | Healthy canary | Watch-for (incident signal) |
|---|---|---|---|
| `orion_task_cpu_seconds_total{scene_id,scene_version}` | counter | rises gently, well under cap | `rate(...[5m]) > 0.8` sustained 10 min on the canary version → see #89 §2 — scene nearing its cgroup share |
| `orion_task_preempt_total` | counter | a few preempts (time-slicing working) | a *runaway* count climbing without bound = a task that won't terminate within budget repeatedly re-sliced |
| `orion_event_shed_total` | counter | **0** ideally | climbing = events firing faster than the budget drains them, **or** the B5 per-scene task-budget (queued+parked) is full and new fires are shed (validated-but-costly, ADR 006 R-1) — running tasks untouched, but the scene is overloaded. This is also where any "budget exceeded" shows up; there is no separate `budget_exceeded` series. |
| `orion_parked_tasks` | gauge | low, returns to ~0 (delays/awaits resolve) | stuck high / monotonically rising = continuations parked and never resumed. On a live-ops-only canary (§1) this should breathe with `delay`; if it sticks high, a node is hung — and remember an accidental `http.request`/`db.query`/`source.read` parks forever here (§1 hard condition). |
| `orion_timer_wheel_size` | gauge | small, bounded | unbounded growth = timers armed faster than they fire (delay leak) |
| `orion_exec_completion_rejected_total{scene_id,reason}` | counter | **0** | climbing = effect completions arriving for a task/version that no longer exists (stale completion rejected). On a clean live-only canary this should stay 0. |
| `orion_exec_resume_stale_total{scene_id}` | counter | **0** | climbing = resumes dropped because the scene version/epoch moved on (e.g. a re-push or switch mid-flight). Expected to tick once on the rollback switch (§4); steady-state 0. |

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
# 1. drain the canary the authored way — switch active scene to the known-good.
#    THE incident lever. No-kill, no-restart. (auth: -H authorization above)
ssh vps-ovh "curl -fsS -X POST https://zabgate.cyell.dev/orion/api/v1/show/active-scene \
  -H 'authorization: Bearer $OP_TOKEN' -H 'content-type: application/json' \
  -d '{\"scene_id\":\"<ROLLBACK_ID>\"}'"
# switch-away cancels the canary's exec tasks the authored way (ADR 003 §3.1.4
# cancellation-on-switch, #83) — the runaway drains without a process kill.
# Expect a one-shot tick on orion_exec_resume_stale_total (parked continuations
# of the drained version dropped on resume) — that is the cancellation working.

# 2. (optional, if the canary version is genuinely pathological) archive it so
#    it cannot be re-aired by accident:
ssh vps-ovh "curl -fsS -X POST https://zabgate.cyell.dev/orion/api/v1/scenes/<CANARY_ID>/status \
  -H 'authorization: Bearer $OP_TOKEN' -H 'content-type: application/json' \
  -d '{\"status\":\"archived\"}'"
```

Confirm recovery:

```bash
ssh vps-ovh "curl -fsS -H 'authorization: Bearer $OP_TOKEN' \
  https://zabgate.cyell.dev/orion/api/v1/show"   # active scene == ROLLBACK_ID
ssh vps-ovh "curl -fsS -o /dev/null -w 'health=%{http_code}\n' \
  https://zabgate.cyell.dev/orion/api/v1/health"  # 200 (health is unauthenticated)
ssh vps-ovh "docker stats --no-stream --format 'cpu={{.CPUPerc}}' orion"  # drops back toward idle (~2%)
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

> Gateway calls (`/show`, `/show/active-scene`, `/scenes/*`) need
> `-H "authorization: Bearer $OP_TOKEN"`; `/health` + `/ready` do not. Metrics
> route is `/orion/internal/metrics` (the `/orion` prefix matters).

| Check | Command | Healthy |
|---|---|---|
| Canary live | `GET /orion/api/v1/show` (auth) | active == `CANARY_ID` |
| CPU cap holding | `docker stats --no-stream orion` | CPU% « 200%, host OK |
| Budget breaches / shed | `orion_event_shed_total` (no `budget_exceeded` series exists) | 0 |
| Events shed | `orion_event_shed_total` | 0 |
| Parked tasks | `orion_parked_tasks` | low, returns to ~0 |
| Timer wheel | `orion_timer_wheel_size` | small, bounded |
| Stale completions | `orion_exec_completion_rejected_total` | 0 |
| Stale resumes | `orion_exec_resume_stale_total` | 0 (one tick on rollback OK) |
| Incident lever | `POST /show/active-scene {scene_id: ROLLBACK_ID}` (auth) | active == `ROLLBACK_ID`, health 200 |
