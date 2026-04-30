# Orion

@../../docs/rules/git.md
@../../docs/rules/security.md
@../../docs/rules/agents.md
@../agents/_shared/architecture.md
@../agents/_shared/conventions.md
@../agents/_shared/deploy.md
@../agents/_shared/projects.md

## Description

Orion is the Zablab platform's interface to Twitch. It owns :

- **Twitch credentials** — AES-GCM encrypted stream keys + optional Helix OAuth tokens.
- **Helix OAuth flow** — `/api/v1/twitch/oauth/authorize` + `/api/v1/twitch/oauth/callback`.
- **IRC chat plumbing** — capture (when a future supervisor is wired) + WS fan-out for blueprint consumers.

It does **not** own broadcast media. Streaming runs through [Pulsar](https://github.com/ZabLaboratory/Pulsar) (broadcast engine bundled in Prism), which pushes directly to Twitch RTMP. Pulsar is an external module — Orion never reaches into Pulsar's process, and Pulsar never reaches into Orion's infrastructure.

### Future scope (separate PRs)

- **EventSub webhooks** — subs / donations / bits / follows / raids / hype train.
- **Expanded Helix endpoints** — channel info, schedule, clips, predictions.
- **Subscriber-driven IRC supervisor** — re-introduces chat capture when a WS client subscribes to `/api/v1/chat/live/{channel}`. The previous supervisor (which reconciled live streams ↔ IRC connections) was removed at the v0.4.0 pivot because Orion no longer knows what a stream is.

## Stack

- **Runtime**: Python 3.11
- **Framework**: FastAPI + uvicorn
- **ORM**: SQLAlchemy 2 async
- **Migrations**: Alembic
- **DB**: PostgreSQL 16
- **Crypto**: `cryptography` (AES-GCM 256-bit) for stream keys + OAuth tokens
- **Package manager**: `uv`
- **Linter / formatter**: `ruff`
- **Type checker**: `mypy` (strict)

## Setup local

```bash
uv sync
uv run uvicorn orion.main:app --reload --port 4007

# Env file lives at etage 1 — ../.env.orion
# ENCRYPTION_KEY must be a base64-urlsafe 32-byte value:
#   python -c "import secrets,base64; print(base64.urlsafe_b64encode(secrets.token_bytes(32)).decode())"

# Docker (requires the shared network first)
docker network create zab_network   # one-off
docker compose up -d

# DB migration
uv run alembic upgrade head
```

## Architecture

Orion is strictly gateway-first: every HTTP client reaches it via ZabGate (`/orion/*`).

### Data model

| Table | Role |
|---|---|
| `twitch_credentials` | Per-user destination: AES-GCM encrypted stream key + optional OAuth |
| `chat_messages` | Captured Twitch IRC messages for replay / audit (channel-keyed) |

Tables that existed before the v0.4.0 pivot and have been retired : `streams`, `stream_metrics`, `stream_destinations`. The `chat_messages.stream_id` FK was dropped as part of the same migration — chat is channel-keyed only ; analytics group by `channel` and `sent_at`.

### Endpoints

All under `/api/v1` (gateway strips `/orion`):

- `GET /health` — liveness + DB check
- `GET /api/v1/credentials` / `POST` / `PUT/DELETE /{id}` — credentials CRUD (secrets never exposed)
- `POST /api/v1/twitch/oauth/authorize` / `callback` — Helix OAuth flow
- `WS /api/v1/chat/live/{channel_login}` — live chat events (idle until the new supervisor lands)
- `GET /api/v1/_schema` — QueryMe catalogue (read-only, blueprint-facing — only `chat_messages` exposed)
- `POST /api/v1/_query` — QueryMe execution (read-only)

### Folder layout

```
Orion/
├── .github/
│   ├── CODEOWNERS
│   └── workflows/
│       ├── ci.yml
│       └── deploy.yml
├── alembic/
│   ├── env.py
│   ├── script.py.mako
│   └── versions/
│       ├── 20260424_0001_init_orion_schema.py
│       ├── 20260425_0002_pivot_to_overlays.py
│       ├── 20260425_0003_overlay_playlist.py
│       ├── 20260426_0004_stream_record_flag.py
│       ├── 20260426_0005_stream_destinations.py
│       └── 20260430_0006_drop_streaming_surface.py   ← v0.4.0 pivot
├── src/orion/
│   ├── main.py
│   ├── config.py
│   ├── database.py
│   ├── models/{base,chat,credential}.py
│   ├── schemas/credential.py
│   ├── services/{encryption,twitch_helix,twitch_chat,
│   │              credential_service,chat_service,
│   │              db_catalog}.py
│   └── routes/{health,credentials,twitch,chat,internal}.py
├── tests/
├── Dockerfile
├── docker-compose.yml       # dev
├── docker-compose.prod.yml
├── alembic.ini
├── Makefile
├── pyproject.toml
├── uv.lock
└── CLAUDE.md
```

## Conventions

- **Auth**: no local JWT validation. Trust `X-Authenticated-User` injected by ZabGate. Endpoints that mutate per-user state require the header (401 if absent).
- **Secrets never exposed.** `CredentialRead` returns `has_oauth: bool`, never the token itself. Stream keys + OAuth tokens are AES-GCM encrypted ; only the credential and chat services decrypt them, and only in-memory.
- **Read-only blueprint surface.** `_schema` exposes `chat_messages` only ; `twitch_credentials` is intentionally absent. Adding a new table to the catalogue requires an explicit security review (no automatic exposure).

## CI/CD

- **Push / PR**: `ci.yml` runs `ruff`, `mypy`, `pytest`, `pip-audit`, TruffleHog secret scan, `uv lock --check`, CODEOWNERS presence.
- **Merge on main**: `deploy.yml` rsyncs to the VPS, writes `.env`, rebuilds the image, runs `alembic upgrade head`, restarts the container, checks `/health` via the internal network, smoke-tests `/health` via ZabGate.

### Required GitHub Secrets

| Secret | Role |
|---|---|
| `VPS_HOST`, `VPS_USERNAME`, `VPS_SSH_KEY`, `VPS_APP_PATH` | SSH deployment |
| `ORION_PG_PASSWORD` | Postgres password |
| `ORION_ENCRYPTION_KEY` | AES-GCM key (32 bytes, urlsafe-b64) |
| `TWITCH_CLIENT_ID`, `TWITCH_CLIENT_SECRET` | Twitch app credentials |
| `ORION_TWITCH_OAUTH_REDIRECT_URI` | e.g. `https://zablab.cyell.dev/orion/credentials` |

Removed at the v0.4.0 pivot : `ORION_PUBLIC_BASE_URL`, `ORION_PUBLIC_WHIP_BASE`, MEDIAMTX_*.

## Resolution criteria

Conforme à `docs/rules/git.md`. Une branche est **résolue après merge** sur `main` quand :

1. **Squash merge effectué** sur `main` par le mainteneur.
2. **Tous les jobs CI verts** sur le commit de merge.
3. **Déploiement sur le VPS** terminé sans rollback, `/health` répond 200 via l'intranet Docker et via ZabGate.
4. **Migration appliquée** sans erreur (`alembic upgrade head`).
5. **Aucune régression** `.health.json` niveau `critical` ou `high`.
6. **Branche supprimée** du remote après le squash.
