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

## Playwright / Chromium deps

> Track: discovered on Solar CI (`e2e` job, `chrome-headless-shell` launch).
> Owner: Keeper. Same pool, same image (`zab-org-runner:pg`) — additive bake,
> not a separate image.

### Symptom

```
error while loading shared libraries: libnspr4.so: cannot open shared object file: No such file or directory
```

`chrome-headless-shell` (Playwright) fails to launch on every job — the base
image ships no Chromium runtime libs (`libnspr4`, `libnss3`, `libasound2`, …).

### Root cause

- Playwright's `--with-deps` install path is unusable: the JIT job container's
  rootfs is mounted `nosuid`, and `sudo` — despite the baked NOPASSWD sudoers
  from the PostgreSQL fix above — **cannot elevate on a `nosuid` mount**. Any
  runtime `apt-get install` (via `--with-deps` or a workflow step) fails the
  same way the PG apt-get did, for a different reason (mount flag, not missing
  binary).
- Net effect: the Chromium runtime libs must be **pre-baked into the image**,
  same pattern as PostgreSQL — no apt call can happen at job time regardless
  of privilege.

### Fix

Baked into the same `/home/ubuntu/zab-org-runners/image/Dockerfile`, additive
to the PostgreSQL layer:

```dockerfile
RUN apt-get update -qq && \
    apt-get install -y -qq --no-install-recommends \
        libnspr4 libnss3 libasound2 && \
    rm -rf /var/lib/apt/lists/*
```

Rebuild + swap, same procedure as the PostgreSQL cutover:

```bash
ssh vps-ovh
cd /home/ubuntu/zab-org-runners/image
cp Dockerfile Dockerfile.bak.pre-playwright     # backup (rollback)
# edit Dockerfile: add the libnspr4/libnss3/libasound2 layer
sudo docker build --load -t zab-org-runner:pg -f Dockerfile .
sudo docker images zab-org-runner:pg            # confirm it's in the daemon
cd /home/ubuntu/zab-org-runners
export APP_PRIVATE_KEY="$(cat /home/ubuntu/runner-orchestrator-zab/secrets/app_private_key.pem)"
sudo -E docker compose up -d                    # recycle the pool empty
```

### Verify after swap

Push a throwaway tag/PR on a repo with an e2e Playwright job (e.g. Solar) and
confirm the job that previously failed on `libnspr4.so` now passes through
browser launch. No dedicated exec probe — the Chromium launch itself is the
verification (unlike PG, there's no long-lived daemon to `pg_ctlcluster` into).

### Rollback

```bash
ssh vps-ovh
cd /home/ubuntu/zab-org-runners/image
cp Dockerfile.bak.pre-playwright Dockerfile
sudo docker build --load -t zab-org-runner:pg -f Dockerfile .
cd /home/ubuntu/zab-org-runners
export APP_PRIVATE_KEY="$(cat /home/ubuntu/runner-orchestrator-zab/secrets/app_private_key.pem)"
sudo -E docker compose up -d
```

Rolling back re-opens the `libnspr4.so` failure for every Chromium-based e2e
job org-wide (Solar and any future consumer). Only roll back if the new layer
itself breaks image build or runner registration.

---

## PostgreSQL 16 migration (2026-08-11, ADR 018 §3.2, Orion #317)

> Track: `ZabLaboratory/Orion:docs/adr/018-ci-postgres-substrat-runner-zab.md`
> (merge `af78d5d9`). Owner: Keeper.

### Root cause

`postgresql` on the base image's OS defaults to whatever the distro's own
package repo ships — Ubuntu 20.04 focal ships **PostgreSQL 12**. A
migration-parity job asserting against cluster `16 main` failed outright
(`specified cluster '16 main' does not exist`): the CI runner's PG major
never matched prod. Bump the version, not the bake.

**Focal has no PG16 path.** PGDG dropped `focal-pgdg` entirely (no `Release`
file, no archive fallback) — `postgresql-16` is not installable on Ubuntu
20.04 at all, from any repo. The base image had to move to
`myoung34/github-runner:ubuntu-jammy` (22.04), the nearest base PGDG still
publishes for. Every package this image already depended on kept its name
across the bump except `libasound2` (renamed `libasound2t64` on 24.04, not
22.04 — no change needed).

### Fix

`image/Dockerfile` now: adds the PGDG apt repo (key + `$(lsb_release
-cs)-pgdg` source) on top of `ubuntu-jammy`, installs
`postgresql-16 postgresql-client-16`, and carries
`LABEL org.opencontainers.image.revision="<ADR-018-merge-sha>"`. Because the
base image changed wholesale (fresh jammy rootfs), there is no PG12
cohabitation to strip — `pg_lsclusters` reports a single `16 main` line by
construction (ADR §3.2 R-6 guard).

### Procedure followed

```bash
ssh vps-ovh
cd /home/ubuntu/zab-org-runners/image
cp Dockerfile Dockerfile.bak.<date>                  # backup (rollback)
cp Dockerfile.pg16 Dockerfile                        # promote the PG16 draft
docker build --load -t zab-org-runner:pg16 -f Dockerfile .
docker run --rm --entrypoint pg_lsclusters zab-org-runner:pg16   # 1 line, ver 16
docker image inspect zab-org-runner:pg16 \
  --format '{{index .Config.Labels "org.opencontainers.image.revision"}}'

cd /home/ubuntu/zab-org-runners
cp docker-compose.yml docker-compose.yml.bak.<date>  # backup (rollback)
# docker-compose.yml: image: zab-org-runner:pg → zab-org-runner:pg16
./up.sh                                              # NOT `docker compose up -d`
                                                      # directly — up.sh exports
                                                      # APP_PRIVATE_KEY, a bare
                                                      # `docker compose up -d`
                                                      # crash-loops the runners.
```

The JIT layer (`/home/ubuntu/runner-orchestrator-zab/.env`,
`RUNNER_IMAGE=zab-org-runner:pg16`) and the image build itself
(`zab-org-runner:pg16`, single-cluster PG16, labelled) were already in place
from an earlier partial cutover (2026-08-05, `.env.bak-keeper-pg16-20260805-105309`);
this pass closed the remaining gap — the **static** 3-replica layer
(`docker-compose.yml`) was still serving `zab-org-runner:pg` (PG12) until
switched here.

### Verify after swap

```bash
docker ps --format '{{.Names}}\t{{.Image}}\t{{.Status}}' | grep zab-org-runner
# all 3 zab-org-runners-zab-org-runner-{1,2,3}-1 on zab-org-runner:pg16, Up
```

A non-DB job (any `pull_request`-triggered `ci.yml` run against the static
pool) going green post-switch is the acceptance proof (ADR RC 9); a
sustained 2 h no-`queued`-over-10-min window is a longer-horizon check, not
verifiable inside a single operator session — monitor via `gh run list`
after cutover if a regression is suspected.

### Regression found during rollout: `e2e (Postgres)` — sudo broke ("no new privileges")

First real CI run on the switched pool (`ZabLaboratory/Orion#323`) showed
PG16 correctly served (`PostgreSQL already provisioned (16 main) —
skipping apt`) but the privilege-aware `pg_ctlcluster` step then failed:

```
sudo: The "no new privileges" flag is set, which prevents sudo from running as root.
```

**Not** a `security_opt`/`no-new-privileges` Docker flag — confirmed absent
from `docker-compose.yml`, and `NoNewPrivs: 0` on the idle container's own
PID 1 (`docker exec … cat /proc/1/status`). The flag only appears on the
process the Actions runner spawns **for the job step itself**.

Root cause: the base bump to `myoung34/github-runner:ubuntu-jammy` (forced
by PGDG dropping focal, see above) pulled a newer Actions runner binary
(`Runner.Listener --version` → `2.336.0`) than whatever was baked into the
long-cached `:latest`/focal image. Newer runner releases harden job
execution when the runner process itself starts **as root**
(`RUN_AS_ROOT=true`, the image's default — see `/entrypoint.sh`): before
executing a job step they internally drop to the unprivileged `runner`
user via a syscall path that also sets `PR_SET_NO_NEW_PRIVS` on that
process tree, as a defense against a job step using a setuid/NOPASSWD-sudo
binary to climb back to root. That drop-and-harden path is new behaviour
in the newer runner binary, not something the image or compose file
configures directly.

**Fix — `RUN_AS_ROOT: "false"`** in `docker-compose.yml`'s runner
environment block. With this set, `/entrypoint.sh` execs
`gosu runner ./bin/Runner.Listener …` directly (see its `else` branch) —
the Listener (and everything it forks, including job steps) runs as
`runner` (uid 1001) **from process start**, so there is no root→job
internal transition for the runner's hardening logic to intercept, and
`sudo` (NOPASSWD, baked into the image) works exactly as it did on the old
focal/PG12 image. Verified live: `ps -ef` inside a recreated container
shows `Runner.Listener` owned by `runner`, not `root`; `e2e (Postgres)`
went green on rerun (`ZabLaboratory/Orion#323`, run `31448903768`, job
`93652608776`, 1m41s).

This is a **durable** config, not a workaround: `RUN_AS_ROOT=false` is a
documented, supported mode of the `myoung34/github-runner` image (least
privilege — the container never needs root once PG/sudo/Chromium libs are
pre-baked at build time), and it is strictly a security improvement over
the previous root-by-default posture. Scope: this env var lives on the
shared anchor (`&r`) in `docker-compose.yml`, so it applies fleet-wide to
all 3 static replicas — every ZabLab repo's CI on this pool now runs job
steps as non-root. Only Orion's own checks were exercised as proof
(8/8 green including `e2e (Postgres)`); no other repo's workflow was
audited for an implicit root assumption — flag a regression here if one
surfaces on a different repo's CI post-cutover.

```bash
# applied together with the pg16 image switch, same session:
cp docker-compose.yml docker-compose.yml.bak.norunasroot.<date>
# add under the `&r` anchor's `environment:` block:
#   RUN_AS_ROOT: "false"
./up.sh
```

Rollback: drop the `RUN_AS_ROOT: "false"` line (or restore
`docker-compose.yml.bak.norunasroot.<date>`) and `./up.sh` — reverts to
root-by-default, which re-opens this exact regression on any job that
needs sudo. Only roll back if `RUN_AS_ROOT=false` itself breaks runner
registration; it does not affect PG16 or the OCI label.

### Rollback

```bash
ssh vps-ovh
cd /home/ubuntu/zab-org-runners/image
cp Dockerfile.bak.<date> Dockerfile      # back to focal + PG12
docker build --load -t zab-org-runner:pg -f Dockerfile .
cd /home/ubuntu/zab-org-runners
cp docker-compose.yml.bak.<date> docker-compose.yml   # image: zab-org-runner:pg
./up.sh
```

The previous `zab-org-runner:pg` (PG12) image stays present locally — no
pull needed. Only roll back if the pg16 image itself breaks runner
registration; an ADR-scope regression is a new issue, not a revert target.

---

## Maintenance

- The image pins `myoung34/github-runner:latest` as its base. To pick up a
  new base release, rebuild (`--load`) and `compose up -d` again.
- Keep `docs/runbooks/ci-runner-pg-image.md` (this file) in sync if the
  image gains more pre-baked tooling; the runner stack itself
  (`/home/ubuntu/zab-org-runners`) is not under git.
- The complementary ci.yml step is in `.github/workflows/ci.yml` (job `e2e`).
- Chromium/Playwright libs (`libnspr4`, `libnss3`, `libasound2`) are baked
  alongside PostgreSQL — one image, two independent additive layers.
- **Post-2026-08-11**: the pinned major is PostgreSQL 16 (`ubuntu-jammy`
  base); see the migration section above for the focal→jammy rationale and
  the PGDG repo wiring.
