# Runbook — local execution stack (Orion engine, no antenna)

Goal: run the Orion reactive engine **end-to-end on a dev machine**, with its
full upstream chain, so a scene (blue + bindings + `db.query`) can be exercised
**without Twitch / Pulsar**. This is local dev infra, not the VPS.

Chain brought up: **ZabGate** (auth + proxy) · **ZabAuth** (login / operator) ·
**ZabTruth** + **ZabRanking** (the `db.query` data sources) · **Orion** (engine),
each with its own Postgres 16. Quasar and Pulsar are intentionally absent —
events are injected through the channels in §6 instead of the Twitch transport.

All commands run from the **structure root** `D:\Documents\Zab` unless noted.

---

## 0. Prerequisite — a container runtime (BLOCKER on a fresh machine)

The stack is Docker-only. As of 2026-06-21 the dev machine has **no container
runtime**: no Docker Desktop, no `docker`/`docker-compose` on PATH, no WSL2
distro. Nothing below runs until one of these is installed:

- **Docker Desktop for Windows** (simplest — bundles compose v2 + a Linux VM), or
- **Docker Engine inside a WSL2 distro** (`wsl --install`, then install Docker
  Engine in the distro and run these commands from the WSL shell, pointing the
  build contexts at the `/mnt/d/Documents/Zab/...` paths).

Verify before proceeding:

```bash
docker version          # must print a Server version
docker compose version  # compose v2
```

One-off shared network (idempotent):

```bash
docker network create zab_network   # ignore "already exists"
```

---

## 1. Secrets (étage 1 — never committed)

Every service reads `D:\Documents\Zab\.env.<service>`. These already exist and
are complete (verified 2026-06-21): `.env.zabgate`, `.env.zabauth`,
`.env.zabtruth`, `.env.zabranking`, `.env.orion`. Key invariants:

- `JWT_SECRET` is **identical** in `.env.zabgate` and `.env.zabauth` (HS256
  shared secret) — do not desync them.
- `.env.orion` ships a valid admin `ORION_OPERATOR_TOKEN` (signed with that
  same secret) and `ORION_DATASOURCES=truth=truth,ranking=ranking`, so Orion's
  `db.query` resolves through the gateway out of the box.
- The étage-1 `.env.*` hostnames (`zabgate:4000`, `zabauth-postgres:5432`, …)
  match the Docker aliases in the localstack compose — no rewrite needed.

If any `.env.<service>` is missing, create it from that repo's `.env.example`
and fill the secrets — **do not invent credentials**, ask the porteur.

---

## 2. Bring the stack up

The orchestration file is `Orion/docker-compose.localstack.yml`. It unifies the
five repos on `zab_network`, publishes host ports for debug, and (unlike Orion's
own `docker-compose.yml`, which only publishes the DB) publishes Orion's API on
`4007`.

```bash
cd D:/Documents/Zab
docker compose -f Orion/docker-compose.localstack.yml up -d --build
docker compose -f Orion/docker-compose.localstack.yml ps
```

Host ports: ZabGate `4000`, Orion API `4007`; Postgres `5441` (auth) / `5442`
(truth) / `5443` (ranking) / `5447` (orion).

---

## 3. Migrations

- **Orion** runs `goose up` automatically at boot (distroless entrypoint) — no
  manual step. Confirm via `GET /ready` in §5.
- **Python services do NOT migrate on boot** (their CMD is plain `uvicorn`). Run
  Alembic once per service after first `up`:

```bash
docker compose -f Orion/docker-compose.localstack.yml exec zabauth    alembic upgrade head
docker compose -f Orion/docker-compose.localstack.yml exec zabtruth   alembic upgrade head
docker compose -f Orion/docker-compose.localstack.yml exec zabranking alembic upgrade head
```

---

## 4. Create a local operator

`/operator/*` and the cockpit require role `operator`/`admin`. Seed it
idempotently (reads creds from env, never hard-coded — `ZabAuth/scripts/seed_operator.py`):

```bash
docker compose -f Orion/docker-compose.localstack.yml exec \
  -e ZABAUTH_SEED_OPERATOR_EMAIL="$ZABLAB_OPERATOR_EMAIL" \
  -e ZABAUTH_SEED_OPERATOR_PASSWORD="$ZABLAB_OPERATOR_PASSWORD" \
  zabauth python -m scripts.seed_operator
```

Source the credentials from étage-1 `.env.zablab-operator` first
(`set -a; . ../.env.zablab-operator; set +a`). Alternatively, register a user
via the public route `POST http://localhost:4000/auth/api/v1/auth/register`
then promote it (operator-gated `POST /auth/users`). Get a JWT for later calls:

```bash
curl -sf -X POST http://localhost:4000/auth/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"'"$ZABLAB_OPERATOR_EMAIL"'","password":"'"$ZABLAB_OPERATOR_PASSWORD"'"}'
```

---

## 5. Seed the test data (so `db.query` returns real rows)

`ZabTruth/scripts/seed_draft_test_data.sh` imports two finished Leaguepedia
series (LCK Finals HLE vs Gen.G, LEC Finals MKOI vs G2) into ZabTruth and
bulk-rates game-1 players in a ZabRanking split. **Point it at the local
gateway**, not prod:

```bash
cd D:/Documents/Zab/ZabTruth
ZAB_ENV=../.env.zablab-operator GATE=http://localhost:4000 \
  bash scripts/seed_draft_test_data.sh
```

> The seed script currently lives only on branch `conduit/seed-draft-test-data`
> (commit `32892ca`) and is **not on any remote** — check it out locally first.
> It echoes the LCK/LEC game-1 match UUIDs (used by `db.query` selectors).
> It needs the Leaguepedia browse surface (public GET via ZabGate), so the
> import works fully offline-of-Twitch.

---

## 6. Prove execution + injection channels

First, health (engine is alive + migrated + has a scene roster):

```bash
curl -sf http://localhost:4007/api/v1/health     # liveness → 200
curl -sf http://localhost:4007/api/v1/ready       # readiness: DB ping + roster
# via gateway (auth path):
curl -sf http://localhost:4000/orion/api/v1/health
```

Three ways to inject an event locally, **no Twitch**:

| # | Channel | When to use | Dependency |
|---|---|---|---|
| **A** | `POST /api/v1/validate/simulate` | fastest engine smoke; compiles a Blue graph in-body and runs it (ADR 015 Amdt 1) | **none** — no scene push, no datasource needed |
| **B** | WS write to `__inputs.platform.twitch.*` on a test session | simulate a chat/EventSub leaf write and watch the delta | scene pushed + active + `WS /scenes/{id}/test` |
| **C** | `POST /api/v1/operator/call/{blueprint_id}/{entrypoint}` | fire an operator spine (`core.operator.on-call@1`), active-only (409 if dormant) | scene pushed + active + operator JWT |

**Recommended smoke (channel A — proves the engine EXECUTES, zero infra deps):**

```bash
curl -sf -X POST http://localhost:4007/api/v1/validate/simulate \
  -H 'Content-Type: application/json' \
  -d @- <<'JSON'
{ "service": "truth",
  "graph": { "...": "a trivial on-event blueprint with a core.output@1 leaf" } }
JSON
```

A 200 with a populated output delta proves the compiler + scheduler + executor
path runs locally. For the **full data path** (proves `db.query` against the
seeded rows), use channel B/C against a pushed scene whose blueprint reads
`db.query truth`/`ranking` — follow the activation sequence in
`agents/_shared/live-testing.md` §2 (create blueprint → store layout with the
**full 64-hex** `canvas_version` → push → validate → activate), then inject and
read the leaf delta off the test-session WS.

---

## 7. Tear down

```bash
cd D:/Documents/Zab
docker compose -f Orion/docker-compose.localstack.yml down          # keep volumes
docker compose -f Orion/docker-compose.localstack.yml down -v       # wipe DBs too
```

---

## Notes / known gaps

- **No Quasar/Cosmos/Pulsar** in this stack by design — Twitch transport is
  replaced by the injection channels above. Add them only to test the transport.
- The draft seed branch is not pushed to any remote (see §5) — a clean clone
  will not have it until `conduit/seed-draft-test-data` lands on `origin`.
- This runbook is descriptive infra; the compose touches no committed secret.
