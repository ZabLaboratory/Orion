# Runbook — Solar bundle deploy + LSDP `dual` flip (Orion)

> Track infra M8 · Pulsar #48 · ADR 007 §C.2/§C.5.
> Owner: Keeper. Surface: VPS `vps-ovh` (`ubuntu@51.91.126.43`), app path
> `/home/ubuntu/orion`, container `orion` on `zab-internal` (gateway-only via
> ZabGate at `https://zabgate.cyell.dev/orion`).

This runbook covers two independent operations that ship together:

1. **Deploy a versioned Solar bundle** under `ORION_SOLAR_ROOT/<version>/`,
   served at `/orion/static/solar/<version>/*`.
2. **Flip `ORION_LSDP_MODE=dual`** to activate LSML persist/serve + the
   `/show/stream.lsdp` route (additive; bespoke `/show/stream` untouched).

Each is reversible on its own. They are documented together because the M8
deliverable lands both.

---

## 1. Deploy a Solar bundle (`vX.Y.Z`)

### Preferred path — CI workflow (repeatable)

`.github/workflows/solar-deploy.yml` (Orion), `workflow_dispatch`:

```
gh workflow run solar-deploy.yml -R ZabLaboratory/Orion \
  -f solar_version=v0.2.0
```

What it does, in order:
1. **Preflight**: validates the version is semver, the VPS secrets exist
   (`VPS_HOST`, `VPS_USERNAME`, `VPS_SSH_KEY`, `VPS_APP_PATH`), and that the
   `solar-<version>.tgz` asset is reachable on the public `ZabLaboratory/Solar`
   release. Fails fast before touching the box.
2. **Install** (idempotent): SSHes to the VPS, downloads the tarball
   anonymously over HTTPS, extracts into a staging dir, asserts `index.html`
   at root, atomically swaps it into `ORION_SOLAR_ROOT/<version>/`, and writes
   a `.installed` marker **last** (the success signal). A version already
   marked `.installed` is a no-op. `force_reinstall=true` clears the marker
   first (repair a corrupt install).
3. **Health-check via gateway**: `GET https://zabgate.cyell.dev/orion/static/solar/<version>/index.html`
   must return a final `200` (follows Go's `301 /index.html → ./` redirect).

No `continue-on-error`; any step failing fails the run. Secrets come from
GitHub Secrets — never in plaintext.

### Why this lives in Orion (not Pulsar)

Orion owns `ORION_SOLAR_ROOT`, the `./solar` host dir bind-mounted read-only
into the `orion` container (`docker-compose.prod.yml`), and the static file
server (`internal/api/static.go`). Pulsar/browser hosts only *consume* the
served bundle by URL. The "which Solar version is on the box" handle is an
Orion operational concern.

### Manual fallback (if CI is unavailable)

```bash
V=v0.2.0
ssh vps-ovh "
  set -euo pipefail
  target=/home/ubuntu/orion/solar/\$V
  mkdir -p \"\$target\"
  tmp=\$(mktemp -d)
  curl -fsSL --retry 3 -o \"\$tmp/s.tgz\" \
    https://github.com/ZabLaboratory/Solar/releases/download/\$V/solar-\$V.tgz
  tar -xzf \"\$tmp/s.tgz\" -C \"\$target\"
  test -f \"\$target/index.html\"   # refuse if no index.html
  touch \"\$target/.installed\"
  rm -rf \"\$tmp\"
"
# verify via gateway:
ssh vps-ovh "curl -fsSL -o /dev/null -w '%{http_code}\n' \
  https://zabgate.cyell.dev/orion/static/solar/$V/index.html"   # want 200
```

The bundle is mounted read-only into the container already (the bind mount in
`docker-compose.prod.yml`); **no container restart is needed** to serve a new
Solar version — the static handler reads the live host directory.

### Rollback (Solar bundle)

Versions are path-immutable and additive — installing `v0.2.0` does not touch
`v0.1.1`. Consumers pin a version in the URL. To roll a consumer back, point
it at the previous version's path. To remove a bad bundle entirely:

```bash
ssh vps-ovh "rm -rf /home/ubuntu/orion/solar/v0.2.0"
```

This only affects clients requesting that exact version path; no restart.

---

## 2. Flip `ORION_LSDP_MODE=dual`

### Effect

- `bespoke` (default / absent key): LSML neither persisted on push nor served;
  `/show/stream.lsdp` route **not registered** (404 at the mux). No-op.
- `dual`: on push, the compiler ALSO persists the LSML 1.1 bundle into
  `scene_pushed_versions.lsml_bundle_jsonb/_hash` (migration 0002, already
  applied); served at `GET /api/v1/scenes/{id}/lsml-bundle?v=<hash>` under the
  **same gating as the bespoke render-bundle** (both gateway-gated, no extra
  local auth middleware — see `internal/api/public.go`); and the
  `/api/v1/show/stream.lsdp` WS route is registered beside the untouched
  bespoke `/show/stream`.

CC-2 (Bastion) honored: no new host port (Orion stays `expose`-only on
`zab-internal`, gateway-only via ZabGate); the LSML endpoint reuses the
render-bundle gating (not a new unauthenticated surface); the flip is a single
env key — removing it returns to bespoke (no-op).

### Apply

Etage-1 secret file `D:\Documents\Zab\.env.orion` (never committed) is the
source of truth; the VPS `/home/ubuntu/orion/.env` is what the container reads.
Add/set the key in both, then recreate the container:

```bash
# 1. Etage-1 (local):   add  ORION_LSDP_MODE=dual  to D:\Documents\Zab\.env.orion
# 2. VPS .env:
ssh vps-ovh "
  cd /home/ubuntu/orion
  cp .env .env.bak-\$(date +%Y%m%d-%H%M%S)            # rollback snapshot
  grep -q '^ORION_LSDP_MODE=' .env \
    && sed -i 's/^ORION_LSDP_MODE=.*/ORION_LSDP_MODE=dual/' .env \
    || printf '\nORION_LSDP_MODE=dual\n' >> .env
  docker compose -f docker-compose.prod.yml up -d --force-recreate orion
"
```

> **Durability across code deploys.** `ci.yml`'s "Write remote .env" step
> regenerates `/home/ubuntu/orion/.env` on every push to main. It now templates
> `ORION_LSDP_MODE` from the **`ORION_LSDP_MODE` repo variable** (defaulting to
> `bespoke` when unset). To keep the `dual` flip durable, set the repo variable:
>
> ```
> gh variable set ORION_LSDP_MODE -R ZabLaboratory/Orion --body dual
> ```
>
> Without this, a code redeploy reverts the manual VPS flip to bespoke. The
> etage-1 `.env.orion` remains the human-readable record of intent.

### Verify

```bash
ssh vps-ovh "
  # readiness 200 via gateway
  curl -fsS -o /dev/null -w 'ready=%{http_code}\n' https://zabgate.cyell.dev/orion/api/v1/ready
  # .lsdp route registered → no longer 404 at the mux. 401/426 (auth/upgrade
  # required) means routed OK; 404 means still bespoke / not registered.
  curl -s -o /dev/null -w 'lsdp=%{http_code}\n' https://zabgate.cyell.dev/orion/api/v1/show/stream.lsdp
  # bespoke stream still routed (additive, non-regressive)
  curl -s -o /dev/null -w 'stream=%{http_code}\n' https://zabgate.cyell.dev/orion/api/v1/show/stream
"
```

LSML persist storage present (migration 0002):

```bash
ssh vps-ovh "docker exec orion-postgres psql -U orion -d orion -tAc \
  \"SELECT column_name FROM information_schema.columns \
    WHERE table_name='scene_pushed_versions' AND column_name LIKE 'lsml_%';\""
# expect: lsml_bundle_jsonb , lsml_bundle_hash
```

### Rollback (LSDP flip → bespoke)

Removing the key (or setting `bespoke`) and recreating returns to the legacy
behaviour. LSML columns stay (nullable, inert); the `.lsdp` route deregisters;
no data loss for bespoke.

```bash
ssh vps-ovh "
  cd /home/ubuntu/orion
  sed -i '/^ORION_LSDP_MODE=/d' .env       # or set =bespoke
  docker compose -f docker-compose.prod.yml up -d --force-recreate orion
"
# and remove the key from D:\Documents\Zab\.env.orion
```

The migration 0002 down-migration (`goose down`) drops the LSML columns/index
if a full schema rollback is ever needed — not required for a mode rollback.

---

## Incident surfaced during the first flip (2026-06-08) — PG password drift

The first `--force-recreate orion` for the flip exposed a **pre-existing**
credential drift unrelated to LSDP: Orion crash-looped with
`failed SASL auth: FATAL: password authentication failed for user "orion"
(SQLSTATE 28P01)`.

**Root cause.** A prior `ORION_PG_PASSWORD` rotation updated the VPS `.env`
(and the CI `ORION_PG_PASSWORD` secret) but was **never applied to the live
`orion-postgres` role** (no `ALTER ROLE`). Orion had stayed up on its open
connection pool, masking the mismatch until a recreate forced re-auth. The
flip did not cause it — it merely detonated a latent landmine that the next
`ci.yml` deploy would have hit anyway.

**Diagnosis (reproducible).** The password the live volume accepts was the one
still recorded in etage-1 `D:\Documents\Zab\.env.orion`; the VPS `.env` held a
diverged value. Proven by auth-testing each candidate:

```bash
ssh vps-ovh "docker run --rm --network zab-internal -e PGPASSWORD='<candidate>' \
  postgres:16-alpine psql -h orion-postgres -U orion -d orion -tAc 'SELECT 1'"
```

**Fix applied (reversible, data-safe).** Rewrote `ORION_PG_PASSWORD` and the
password embedded in `ORION_DATABASE_URL` in the VPS `.env` back to the
live-accepted (etage-1) value, then recreated — no `ALTER ROLE`, no volume
touched, no data loss. Orion went healthy, `lsdp wire enabled mode=dual`.

**Residual / handover to Bastion (credential surface).** The drift means the
`ORION_PG_PASSWORD` GitHub secret and the etage-1 `.env.orion` PG password are
**out of sync with each other**, and a deliberate rotation was left half-done.
Reconciling them is a secrets-surface decision (Bastion):
- Either re-pin the CI secret + etage-1 to the live-accepted value (revert the
  half-rotation), **or**
- Complete the intended rotation: `ALTER ROLE orion PASSWORD '<new>'` on the
  live DB, then align `.env` / CI secret / etage-1 to the new value, in one
  coordinated change.

Until reconciled, **do not** let `ci.yml` rewrite `.env` from the stale
`ORION_PG_PASSWORD` secret (it would re-introduce the 28P01 crash-loop). The
`ORION_PG_PASSWORD` GitHub secret must be corrected to the live-accepted value
**before** the next code deploy.

### Reconciliation — rotation (b), true rotation to a fresh value (2026-06-08, RESOLVED)

An interim option (a) (re-pin the four copies to the live-accepted canonical
value) was considered, then **ruled insufficient by Bastion's conditional
clearance** and superseded by a **true rotation (b)**.

**Why (b) over (a) — chronology, verified by fingerprint.** The transient `sed`
exposure of `ORION_DATABASE_URL` happened during the **first #48 VPS diagnosis,
before any rotation**, when the VPS `.env` already carried the **canonical
`45eeb639…`** value. Proven here: on a fresh non-loopback SCRAM the live role
**accepted** `45eeb639` and **rejected** the pre-#48 `a61f1b51`. So
**canonical == the value that transited the exposed `sed`** → option (a) would
re-adopt a transiently-exposed secret → insufficient under `security.md`.
Bascule to a brand-new value.

**New value.** `python3 -c "import secrets; print(secrets.token_urlsafe(48))"`
generated on the box into a `0600` temp, never printed. Fingerprint
sha256[:16] **`306b7535bc91db33`**, len 64; asserted distinct from canonical
before use.

**Applied — no value ever printed; pipe/stdin only:**
1. `ALTER ROLE orion PASSWORD` on live `orion-postgres` via `psql -v np=…`
   binding fed by stdin (value never in argv/echo). Exit 0.
2. **Fresh non-loopback SCRAM** against `172.26.0.4` on `zab-internal` (NOT
   `127.0.0.1` — container `pg_hba` `trust`s loopback = false PASS):
   new `306b7535` → **SUCCESS**; old canonical `45eeb639` → **28P01** (exposed
   value now **dead** — the security gain of (b)); `wrong-xyz` → **28P01**
   (control negative genuinely challenges).
3. All **four** copies aligned to `306b7535`: live role (above); VPS
   `/home/ubuntu/orion/.env` (`ORION_PG_PASSWORD` **and** the pwd inside
   `ORION_DATABASE_URL`); etage-1 `D:\Documents\Zab\.env.orion` (both fields);
   GH secret `ORION_PG_PASSWORD` via `gh secret set` stdin pipe.
4. **ci.yml path simulated**: the "Write remote .env" heredoc reproduced into a
   throwaway `.env.ci-sim` from the new value; fresh SCRAM via that file's
   `ORION_DATABASE_URL` → OK; goose-style URL connect → OK; file deleted.
5. **Live deploy re-validation (strongest proof of the GH secret).** Re-ran the
   deploy (run `27113773755`) — it wrote the remote `.env` from
   `secrets.ORION_PG_PASSWORD`, recreated Orion, came up `database:ok`. The 4th
   copy is thus proven by a real deploy, not just pipe-provenance.
   `/orion/api/v1/health` & `/ready` = **200**.
6. **Backups purged.** Every `.env.bak-*` was fingerprinted; all carried a
   now-dead password (`a61f1b51`, `45eeb639`) and were removed — only the live
   `.env` (`306b7535`) remains, **no blind-restore landmine**. The `/tmp` secret
   temps (VPS + local) were `shred`-removed.

**Bastion angle — sealed.** The transiently-exposed value no longer
authenticates anywhere; live role + all env copies + CI secret are on a fresh
value that never left a pipe/stdin. No secret value appears in any log, terminal,
commit, or this runbook (fingerprints only).

**Operational rule for next rotation.** Always `ALTER ROLE` the live role in the
**same** change that updates the four env copies. The #48 incident was a rotation
that touched the env copies but never the live role — that asymmetry is the
latent landmine a recreate detonates.

---

## Quick reference — health signals

| Check | Command (via gateway) | Healthy |
|---|---|---|
| Orion liveness | `GET /orion/api/v1/health` | 200 |
| Orion readiness | `GET /orion/api/v1/ready` | 200 |
| Solar bundle served | `GET /orion/static/solar/<v>/index.html` (-L) | 200 |
| LSDP route (dual) | `GET /orion/api/v1/show/stream.lsdp` | not 404 (401/426 = routed) |
| Bespoke stream | `GET /orion/api/v1/show/stream` | not 404 |
| Gateway-only (CC-2) | `docker inspect orion -f '{{.HostConfig.PortBindings}}'` | `map[]` (no host ports) |
