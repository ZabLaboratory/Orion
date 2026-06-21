# Runbook — CI runner image with pre-baked PostgreSQL + sudo

> Track infra · Orion #233 (`e2e (Postgres)` apt-permission flake).
> Owner: Keeper. Surface: VPS `vps-ovh` (`ubuntu@51.91.126.43`), org runner
> stack `/home/ubuntu/zab-org-runners`. Affects the **org-wide** self-hosted
> runner pool — shared by every ZabLab repo's CI.

The org runner pool runs a **custom image** `zab-org-runner:pg`
(`myoung34/github-runner` + pre-baked PostgreSQL + sudo) instead of the
vanilla `myoung34/github-runner:latest`. This runbook records why, how the
swap was done, the two build/compose traps, and the one-command rollback.

---

## Root cause (Orion #233)

`e2e (Postgres)` hard-failed intermittently at ~9 s:

```
E: Could not open lock file /var/lib/apt/lists/lock - open (13: Permission denied)
```

The job's `Start PostgreSQL natively` step ran `apt-get install postgresql`
**at runtime**. Diagnosis on the live runner:

- `myoung34/github-runner` boots as root but runs each **workflow step via
  `gosu runner`** — i.e. the job executes as the non-root user `runner`
  (uid 1001).
- The vanilla image ships **no `sudo` binary** (the `runner` user is in the
  `sudo` group, but the binary is absent).
- Net effect: a runtime `apt-get` runs as non-root with no escalation path →
  Permission denied on the apt lock, **every time on a fresh ephemeral
  runner** (`EPHEMERAL=true`, nothing persists between jobs).

Other PRs went green only when a runner happened to still hold a
previously-installed PostgreSQL. A `services: postgres` container is not an
option — the runner has **no Docker socket** mounted (deliberate, see
`zab-ci-self-hosted-runner-substrate`).

## Fix

Two complementary parts:

1. **ci.yml** (Orion `keeper/ci-e2e-apt-permission`, merged): the PG-start
   step is idempotent + privilege-aware — it **skips apt entirely when
   `pg_ctlcluster` is already present** (the fast path the baked image
   guarantees), uses a sudo shim (empty when root), and routes cluster start
   + bootstrap psql through it. No `continue-on-error`.
2. **Runner image** (this runbook): pre-bake PostgreSQL + sudo so the
   skip-apt fast path is **always** taken — no apt ever runs at job time.

---

## The custom image

`/home/ubuntu/zab-org-runners/image/Dockerfile`:

```dockerfile
FROM myoung34/github-runner:latest
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq && \
    apt-get install -y -qq --no-install-recommends sudo postgresql postgresql-client && \
    rm -rf /var/lib/apt/lists/* && \
    echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-runner && \
    chmod 0440 /etc/sudoers.d/90-runner
```

- `postgresql` + `postgresql-client` → `pg_ctlcluster` present → ci.yml takes
  the skip-apt branch.
- `sudo` + NOPASSWD sudoers for `runner` → the privilege-aware step can start
  the cluster and run the bootstrap psql as the `postgres` OS user.
- Purely **additive** vs the base image — every other repo's CI is unaffected
  (verified: a co-tenant `Deploy` job ran on the swapped pool without
  regression during the cutover).

---

## Build + swap (how it was done)

```bash
ssh vps-ovh

# 1. Build the image INTO the docker daemon. --load is REQUIRED: a plain
#    buildx build exports to the buildx store and the image never lands in
#    `docker images`, so compose can't find it.
cd /home/ubuntu/zab-org-runners/image
sudo docker build --load -t zab-org-runner:pg -f Dockerfile .
sudo docker images zab-org-runner:pg          # confirm it's in the daemon

# 2. Point the compose at the custom image with pull_policy: never.
#    The image is local-only (no registry); without this, `compose up`
#    tries to PULL `zab-org-runner:pg` and fails with `pull access denied`.
cd /home/ubuntu/zab-org-runners
cp docker-compose.yml docker-compose.yml.bak        # backup (rollback)
# docker-compose.yml now sets:  image: zab-org-runner:pg  +  pull_policy: never

# 3. Recreate the pool.
export APP_PRIVATE_KEY="$(cat /home/ubuntu/runner-orchestrator-zab/secrets/app_private_key.pem)"
sudo -E docker compose up -d
```

### Verify after swap

```bash
# Containers on the custom image, healthy:
sudo docker ps --filter name=org-runner --format '{{.Names}}\t{{.Image}}\t{{.Status}}'

# PG + sudo available AS THE RUNTIME USER (uid 1001), not just root:
cid=zab-org-runners-zab-org-runner-1-1
sudo docker exec --user 1001 "$cid" sh -c \
  'command -v pg_ctlcluster >/dev/null && echo PG=yes; sudo -n true && echo SUDO=ok'

# Runners registered + listening (GitHub side):
sudo docker logs --tail=5 "$cid" 2>&1 | grep -i "Listening for Jobs"
```

Expected: image `zab-org-runner:pg`, `PG=yes`, `SUDO=ok`, "Listening for Jobs".

---

## Traps (both hit during the cutover)

| Trap | Symptom | Fix |
|---|---|---|
| `docker build` without `--load` | image absent from `docker images`; `compose up` → `pull access denied` | rebuild with `sudo docker build --load -t zab-org-runner:pg` |
| compose pulls a local-only image | `Image zab-org-runner:pg Error pull access denied ... repository does not exist` (and the `up` interrupts the running pool) | add `pull_policy: never` under the image in the compose anchor |

---

## Rollback (one command path)

Revert the pool to the vanilla image:

```bash
cd /home/ubuntu/zab-org-runners
cp docker-compose.yml.bak docker-compose.yml
export APP_PRIVATE_KEY="$(cat /home/ubuntu/runner-orchestrator-zab/secrets/app_private_key.pem)"
sudo -E docker compose up -d
```

The `e2e (Postgres)` job then falls back to the ci.yml install path (which
only succeeds on a root runner) — so rollback re-opens the original flake.
Only roll back if the custom image itself breaks runner registration; the
fix for an image problem is to rebuild it, not to revert the pool.

---

## Maintenance

- The image pins `myoung34/github-runner:latest` as its base. To pick up a
  new base release, rebuild (`--load`) and `compose up -d` again.
- Keep `docs/runbooks/ci-runner-pg-image.md` (this file) in sync if the
  image gains more pre-baked tooling; the runner stack itself
  (`/home/ubuntu/zab-org-runners`) is not under git.
- The complementary ci.yml step is in `.github/workflows/ci.yml` (job `e2e`).
