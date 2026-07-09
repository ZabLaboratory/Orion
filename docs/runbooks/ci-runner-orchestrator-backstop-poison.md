# Runbook — CI dispatch dead: runner-orchestrator backstop poisoned by a 404 served-repo

> Owner: Keeper. Surface: VPS `vps-ovh` (`ubuntu@51.91.126.43`),
> `/home/ubuntu/runner-orchestrator-zab` (JIT runner orchestrator, `zab-runner-orchestrator`)
> and `/home/ubuntu/zab-org-runners` (static ephemeral org pool).
> Incident date: 2026-07-09. No repo hosts the orchestrator (VPS-only) — its runbooks live here.

## Symptom

Every ZabLaboratory CI job stayed `queued` with `runner=null`; no zablab workflow
had been picked up for ~20 h (last success `ZabCanvas` 2026-07-08 18:18Z). A
`Solar` release (`v0.2.21`, **public** repo, `runs-on: [self-hosted, vps-ovh]`)
sat `queued` indefinitely. `gh run view … --json jobs` → `runner=null labels=null`.

## Diagnosis (reproducible, bounded)

```bash
# runner pool exists & idle?
ssh vps-ovh "docker ps --filter name=runner --format '{{.Names}} {{.Status}}'"
# static ephemeral runners: healthy broker session, or zombie?
ssh vps-ovh "docker exec zab-org-runners-zab-org-runner-2-1 \
  sh -c 'ls -t _diag/*.log|head -1|xargs tail -30' | grep -iE 'broker|error|Listening'"
# does GitHub even see the queued run?
gh api 'repos/ZabLaboratory/Solar/actions/runs?status=queued' --jq '.total_count'
# what the orchestrator actually does with it (the tell): repeated backstop errors
ssh vps-ovh "docker logs zab-runner-orchestrator 2>&1 \
  | grep -iE 'backstop poll error|offering runner|spawned jit|meet' | tail -20"
```

Two independent faults stacked:

1. **Static org pool zombie.** `zab-org-runners-zab-org-runner-2/3` printed
   `Listening for Jobs` but their v2 **broker** session had died
   (`BrokerServer … IOException: Unable to read data from the transport connection`,
   retries exhausted). A `docker restart` reused the stale registration
   (`.runner` `agentId` unchanged) and did **not** fix dispatch. A clean
   `docker compose down && ./up.sh` (which re-mints the GitHub-App registration
   token from `/home/ubuntu/runner-orchestrator-zab/secrets/app_private_key.pem`)
   restored a fresh session — **but** the pool is **org-scoped** and does not
   serve the **public** Solar repo (org runner-group public-repo policy).

2. **JIT orchestrator backstop poisoned (root cause of the wedge).** The real
   dispatcher spawns a **repo-scoped** ephemeral runner per queued job
   (`generate-jitconfig` on `/repos/{repo}/…`) — repo-scoped runners bypass the
   org public-repo restriction, so they *can* serve public Solar. Two spawn paths:
   - `_rediscover_queued_jobs` (120 s): filters by `_labels_match`, which **requires
     the per-repo `zab-<repo>` label** (Amendment 6). Solar's `runs-on:
     [self-hosted, vps-ovh]` lacks `zab-solar` → **silently skipped forever**.
   - `_backstop_poll` (300 s): does **not** label-filter — the only path that can
     serve Solar. But it iterates `RUNNER_SERVED_REPOS` under a **single** try/except
     with **no per-repo isolation**. `zablaboratory/meet` (in the list, App has no
     access) returns **404** → `list_queued_jobs` raises `GitHubAppError` →
     the whole sweep aborts (`backstop poll error — will retry`) **every cycle**,
     before reaching Solar. Net: backstop never spawns anything.

   (`_rediscover_queued_jobs` has per-repo `try/except … continue`, so it survives
   `meet`; the backstop does not — that asymmetry is the bug.)

## Fix applied (hotfix, infra-pure, reversible)

Remove the inaccessible poison repo from the orchestrator's poll list and recreate:

```bash
ssh vps-ovh "
  cd /home/ubuntu/runner-orchestrator-zab
  cp .env .env.bak-keeper-solardeploy-\$(date +%Y%m%d-%H%M%S)   # snapshot
  sed -i 's#,zablaboratory/meet##; s#zablaboratory/meet,##' .env # drop meet
  docker compose -f docker-compose.prod.yml up -d --no-deps --force-recreate zab-runner-orchestrator
"
```

`docker restart` is **not** enough — env is injected at container create; the
`.env` change needs `up -d --force-recreate` to be read.

Verify (backstop first poll is at startup+300 s):

```bash
ssh vps-ovh "docker logs zab-runner-orchestrator --since 6m 2>&1 \
  | grep -iE 'offering runner|spawned jit-solar'"   # want: spawned jit-solar-…
gh run view <run_id> -R ZabLaboratory/Solar --json status   # want: in_progress→completed
```

Result: `spawned jit-solar-b03454e5 — 1/3 slots live`, run `in_progress` → `success`.

Static-pool restore (secondary, done first during diagnosis):

```bash
ssh vps-ovh "cd /home/ubuntu/zab-org-runners && docker compose down && ./up.sh"
```

## Rollback

- Orchestrator config: `cp .env.bak-keeper-solardeploy-<ts> .env` then
  `docker compose -f docker-compose.prod.yml up -d --no-deps --force-recreate zab-runner-orchestrator`.
  Re-adding `meet` only re-introduces the poison — do **not** unless the App is
  actually granted access to `zablaboratory/meet`.
- Static pool: `./up.sh` is idempotent; a bad state is cleared by `docker compose down && ./up.sh`.

## Residual / follow-ups (not done here)

- **Code fix (recommended).** Give `_backstop_poll` the same per-repo `try/except …
  continue` isolation `_rediscover_queued_jobs` already has, so one unreachable
  served-repo can never wedge the whole sweep again. (Orchestrator code is
  VPS-only under `/home/ubuntu/runner-orchestrator-zab/app` — no repo/CI; a code
  change there is a separate, reviewed change, touches App-key surface → Bastion.)
- **`meet` provenance.** `zablaboratory/meet` is in `RUNNER_SERVED_REPOS` but the
  App can't see it (404). Either remove it permanently (done) or install the App
  on it if that repo is meant to be served.
- **Public-repo path.** Solar (public) can only be served by the repo-scoped JIT
  backstop, or by adding the `zab-solar` label to its `runs-on` (would let the
  faster 120 s re-discovery serve it too). The org static pool will not serve it
  under the current runner-group public-repo policy — verifying/relaxing that
  policy needs org-admin.
```
