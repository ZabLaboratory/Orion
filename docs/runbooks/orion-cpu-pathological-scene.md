# Runbook — CPU-pathological scene (Orion exec isolation, R7/B7)

> ADR 003 §3.1.6 / §5 — **R7 / B7**. Issue #89 (B7 isolation volet).
> Owner: Keeper. Surface: VPS `vps-ovh` (`ubuntu@51.91.126.43`), app path
> `/home/ubuntu/orion`, container `orion` on `zab-internal` (gateway-only via
> ZabGate at `https://zabgate.cyell.dev/orion`).

## Doctrine (do not violate)

**Time-slicing protects the scene loop, not the machine.** A scene that passed
the validation gate (#87) can still become CPU-pathological at runtime under
inputs the gate didn't witness. The exec doctrine (ADR 003 §1.1) **forbids
killing, skipping, or amputating** a running scene. The mitigation is therefore
two-layered and contains **no auto-kill**:

1. **Isolate** (OS-level, preventive — this volet): a `cpus` cap on the `orion`
   container's cgroup so a runaway scene saturates **its own** cgroup and
   degrades **its own** scene, never the show, the gateway, or co-tenant
   services on the shared VPS.
2. **Observe + operator decision** (no auto-kill): the per-scene-version metric
   `orion_task_cpu_seconds_total` is an **incident signal**. A human operator
   decides whether to retire the offending scene-version from the air — via the
   validation/status gate (#87), **never** by killing the process.

> **There is no automatic remediation.** Nothing in this runbook kills, restarts,
> or sheds a running task in response to CPU. The container restarts only on
> genuine liveness death (existing `healthcheck`), unrelated to this signal.

---

## 1. Isolation in place (the cgroup cap)

`docker-compose.prod.yml`, service `orion`:

```yaml
deploy:
  resources:
    limits:
      memory: 512M
      cpus: "1.0"
```

`docker compose up` (Compose Spec, the deploy path in `ci.yml`) honours
`deploy.resources.limits.cpus` by translating it to the container's CPU quota
(cgroup `cpu.max`). **1.0 = at most one full core** of host CPU for the entire
`orion` container, no matter how many goroutines a runaway scene spawns.

### Sizing rationale

- The VPS is a **shared multi-service OVH host** (G2 + Zab stacks on one box,
  same IP). A runaway must not starve ZabGate or the live show.
- **1.0 core is far above Orion's normal footprint.** Exec is **dormant in
  prod** (ADR 003 R9): at boot the process does migrations + serves the
  listener — near-idle CPU. The cap does **not** throttle boot, health,
  migrations, or normal serving (verified post-deploy, §4).
- 1.0 is an **absolute** budget, safe regardless of the live core count: on a
  ≥2-vCPU box a single runaway scene leaves ≥1 core for the rest of the host;
  even on the smallest plausible 2-vCPU tier it caps the runaway at half the
  machine, keeping the show responsive.

> **§Sizing — recalibration.** The exact core count was **not confirmable from
> CI** (SSH to the VPS was unavailable when this landed). Confirm with
> `ssh vps-ovh "nproc"` and, if the box is larger (e.g. 4–8 vCPU), the cap may
> be relaxed — but keep enough headroom that a runaway can never approach host
> saturation. The conservative 1.0 is correct until measured.

---

## 2. Alert threshold (incident signal, NOT auto-kill)

No alerting stack lives in this repo, so the threshold is **documented here**
(this runbook is its home until a Prometheus rules file exists in the repo).

- **Metric**: `orion_task_cpu_seconds_total` (counter, labelled by
  scene-version; exported on Orion's internal-only metrics endpoint, exposed by
  the parallel Forge volet).
- **Threshold (PromQL, when an alerting stack lands)**:

  ```
  # Sustained per-scene-version CPU burn = candidate runaway.
  # 0.8 core-seconds/sec averaged over 5 min on a single scene-version,
  # held for 10 min, while the container as a whole approaches its 1.0 cap.
  rate(orion_task_cpu_seconds_total[5m]) > 0.8
  ```

  Rationale: 0.8 core/s on one scene-version means that scene alone is nearing
  the 1.0 container cap — i.e. it is the thing about to be throttled. The 10 min
  `for:` avoids paging on a legitimate transient (a heavy but bounded
  animation). Tune against observed normal once exec runs live.
- **Severity**: warning/incident, **page an operator**. **No automated action.**

When a Prometheus/alerting config is added to the infra, port this rule into it
and update this section to point at the rule's location.

---

## 3. Operator decision path (when the signal fires)

A page on the threshold above means *"a scene-version is burning CPU at its
cap"*, not *"act automatically"*. Steps:

1. **Identify the scene-version.** Read the `scene` / version label on the
   firing `orion_task_cpu_seconds_total` series. Cross-check the active scene:

   ```bash
   ssh vps-ovh "curl -fsS https://zabgate.cyell.dev/orion/api/v1/show"
   ```

2. **Confirm it's the cgroup cap, not the host.** A correctly-isolated runaway
   pins the `orion` container near 100% of its 1.0 core while the host stays
   healthy and the show keeps streaming:

   ```bash
   ssh vps-ovh "docker stats --no-stream orion"   # CPU% ~ 100% of one core
   ssh vps-ovh "uptime"                            # host load not collapsing
   ```

   If the host is fine and only Orion's own scene is laggy, **isolation is
   working as designed** — you have time to decide, no emergency.

3. **Decide: retire the scene-version via the gate (#87), NOT a kill.** If the
   scene is genuinely pathological, take it off the air the authored way —
   switch the active scene away from it and/or archive that version through the
   validation/status surface:

   ```bash
   # switch the live show to a known-good scene (clears the runaway from air):
   ssh vps-ovh "curl -fsS -X POST https://zabgate.cyell.dev/orion/api/v1/show/active-scene \
     -H 'content-type: application/json' -d '{\"scene_id\":\"<good-id>\"}'"

   # then archive the bad version so it cannot be re-activated:
   ssh vps-ovh "curl -fsS -X POST https://zabgate.cyell.dev/orion/api/v1/scenes/<bad-id>/status \
     -H 'content-type: application/json' -d '{\"status\":\"archived\"}'"
   ```

   Switching away cancels that scene's exec tasks the **authored** way (ADR 003:
   cancellation on switch/re-push), draining the runaway without a process kill.

4. **Do NOT** `docker kill`/`restart` `orion` as the remedy. That kills the
   whole show (every scene, every viewer WS), masks the root cause, and violates
   doctrine. Restart only if the container is independently liveness-dead.

5. **Capture for the gate.** Record the offending scene-version hash + inputs;
   it's a candidate to harden the validation gate (#87) so the next push of that
   pattern is caught at authoring, not at runtime.

---

## 4. Post-deploy verification (the cap must not strangle boot/health)

The cap is **preventive** — it ships before exec runs live. It must not regress
the current healthy boot. After the deploy lands `cpus: "1.0"`:

```bash
ssh vps-ovh "
  cd /home/ubuntu/orion
  # cap actually applied to the container's cgroup (NanoCpus = 1.0 core = 1e9):
  docker inspect orion -f 'NanoCpus={{.HostConfig.NanoCpus}}'   # expect 1000000000
  # gateway-only unchanged (no host ports leaked by the deploy change):
  docker inspect orion -f 'Ports={{.HostConfig.PortBindings}}'  # expect map[]
"
# liveness + readiness via gateway — the real boot/health gate:
ssh vps-ovh "curl -fsS -o /dev/null -w 'health=%{http_code}\n' https://zabgate.cyell.dev/orion/api/v1/health"  # 200
ssh vps-ovh "curl -fsS -o /dev/null -w 'ready=%{http_code}\n'  https://zabgate.cyell.dev/orion/api/v1/ready"   # 200
# normal CPU at idle is well under the cap (exec dormant):
ssh vps-ovh "docker stats --no-stream orion"   # CPU% near-idle, nowhere near 100%
```

If `health`/`ready` return 200 and idle CPU is low, the cap is non-regressive.

---

## 5. Rollback

Pure compose change, fully reversible, **no data touched**.

- **Revert the limit** (if the cap ever proved too tight for legitimate prod):
  remove the `cpus: "1.0"` line (or raise it) in `docker-compose.prod.yml`,
  merge, and the next deploy's `up -d --force-recreate orion` drops/relaxes the
  cgroup quota. No volume, no DB, no env change.

- **Emergency relax on the box** (before a code deploy is possible — uncaps
  immediately, survives only until the next deploy rewrites compose state):

  ```bash
  ssh vps-ovh "docker update --cpus 0 orion"   # 0 = no quota (uncapped)
  ```

  Use only if the cap is provably throttling a *legitimate* load and the show
  needs the headroom now; follow up with a compose revert so the change is
  durable and reviewed.

---

## Quick reference — signals

| Check | Command | Expected |
|---|---|---|
| Cap applied | `docker inspect orion -f '{{.HostConfig.NanoCpus}}'` | `1000000000` |
| Gateway-only | `docker inspect orion -f '{{.HostConfig.PortBindings}}'` | `map[]` |
| Liveness | `GET /orion/api/v1/health` | 200 |
| Readiness | `GET /orion/api/v1/ready` | 200 |
| Runaway isolated | `docker stats --no-stream orion` | ~100% of 1 core, host OK |
| Runaway metric | `rate(orion_task_cpu_seconds_total[5m])` per scene-version | `> 0.8` = page operator |
