# Orion

@../../docs/rules/git.md
@../../docs/rules/security.md
@../../docs/rules/agents.md
@../agents/_shared/architecture.md
@../agents/_shared/conventions.md
@../agents/_shared/deploy.md
@../agents/_shared/projects.md

## Description

Orion is the streaming control plane of the Zablab platform. It replaces OBS by
delegating scene composition to the browser (GPU of the streamer's PC) and
relaying the composed feed to Twitch through a MediaMTX container. Orion owns:

- Scenes (canvas layouts persisted as JSONB)
- Twitch credentials (AES-GCM encrypted stream keys + optional Helix OAuth)
- Stream lifecycle (pending → preparing → live → stopping → ended | error)
- MediaMTX path orchestration (control API + dynamic `runOnReady` ffmpeg push)
- Twitch chat prep (IRC client + component registry that references `Blue` blueprints)

Prism (native desktop companion) is the sibling client that wraps ZabView's
broadcaster page with native-GPU conveniences. Orion is indifferent to which
browser posts to its WHIP endpoint.

## Stack

- **Runtime**: Python 3.11
- **Framework**: FastAPI + uvicorn
- **ORM**: SQLAlchemy 2 async
- **Migrations**: Alembic
- **DB**: PostgreSQL 16 (JSONB for scene configs + placeholders)
- **Media engine**: MediaMTX (Go, container) — WHIP ingress, RTMP egress, RTSP
  internal, HLS optional
- **Crypto**: `cryptography` (AES-GCM 256-bit) for stream keys + OAuth tokens
- **Package manager**: `uv`
- **Linter / formatter**: `ruff`
- **Type checker**: `mypy` (strict)

## Setup local

```bash
uv sync
uv run uvicorn orion.main:app --reload --port 4007

# Env file lives at etage 1 — ../.env.orion (copy .env.example and fill)
# ENCRYPTION_KEY must be a base64-urlsafe 32-byte value:
#   python -c "import secrets,base64; print(base64.urlsafe_b64encode(secrets.token_bytes(32)).decode())"

# Docker (requires the shared network first)
docker network create zab_network   # one-off
docker compose up -d

# DB migration
uv run alembic upgrade head
# or, inside the container:
# docker compose run --rm orion alembic upgrade head
```

## Architecture

Orion is strictly gateway-first: every HTTP client reaches it via ZabGate
(`/orion/*`). The one exception is the WHIP ingress — browsers send WebRTC
SDP offers directly to `orion-mediamtx:8889/<path>/whip`, because pushing
`O(GB/h)` of video through ZabGate would be wasteful and pointless (ZabGate is
stateless and doesn't need to see bytes). Every *control-plane* call still
goes through the gateway.

### Flow

```
 Browser (ZabView /broadcast/<streamId>)
   │  1. GET /orion/api/v1/streams/<id>           via ZabGate
   │  2. GET /orion/api/v1/scenes/<id>            via ZabGate
   │  3. POST /orion/api/v1/streams/<id>/start    via ZabGate → Orion API
   │     Orion provisions MediaMTX path, returns whip_url
   │  4. POST <whip_url>  (SDP offer)             direct → MediaMTX :8889
   ▼
 MediaMTX (orion-mediamtx)
   │  5. runOnReady launches ffmpeg
   │     ffmpeg -i rtsp://localhost:8554/<path> -c copy -f flv rtmp://live.twitch.tv/app/<key>
   ▼
 Twitch
```

### Data model

| Table | Role |
|---|---|
| `scenes` | User-authored canvas scene (JSONB config: sources, layout, audio) |
| `twitch_credentials` | Per-user destination: AES-GCM encrypted stream key + optional OAuth |
| `streams` | Broadcast session (one scene + one credential + lifecycle state + MediaMTX path) |
| `chat_components` | Blueprint-backed chat overlays (`blueprint_ref` → Blue) |
| `chat_messages` | Captured Twitch IRC messages for replay/audit |
| `stream_metrics` | Time-series: bitrate, fps, dropped frames, rtt, viewers |

### Endpoints

All under `/api/v1` (gateway strips `/orion`):

- `GET /health` — liveness + DB check
- `GET /api/v1/scenes` / `POST` / `GET/PUT/DELETE /{id}` — scene CRUD
- `GET /api/v1/credentials` / `POST` / `PUT/DELETE /{id}` — credentials CRUD (secrets never exposed)
- `GET /api/v1/streams` / `POST` / `GET /{id}` — stream CRUD
- `POST /api/v1/streams/{id}/start` — provision MediaMTX path, return WHIP URL
- `POST /api/v1/streams/{id}/stop` — teardown
- `POST /api/v1/twitch/oauth/authorize` / `callback` — Helix OAuth flow
- `GET /api/v1/chat/components` / `POST` / `GET/PUT/DELETE /{id}` — chat component CRUD
- `WS  /api/v1/chat/live/{channel_login}` — live chat events
- `GET /api/v1/metrics/streams/{id}` — historical metrics
- `WS  /api/v1/metrics/streams/{id}/live` — live stream state + MediaMTX snapshot

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
│   └── versions/20260424_0001_init_orion_schema.py
├── mediamtx/
│   └── mediamtx.yml         # paths seeded dynamically by Orion
├── src/orion/
│   ├── main.py
│   ├── config.py
│   ├── database.py
│   ├── models/{base,scene,credential,stream,chat,metric}.py
│   ├── schemas/{scene,credential,stream,chat,metric}.py
│   ├── services/{encryption,mediamtx,twitch_helix,twitch_chat,
│   │              stream_manager,scene_service,credential_service,
│   │              chat_service}.py
│   └── routes/{health,scenes,credentials,streams,twitch,chat,metrics}.py
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

- **Auth**: no local JWT validation. Trust `X-Authenticated-User` injected by
  ZabGate. Endpoints that mutate per-user state require the header (401 if absent).
- **Secrets never exposed.** `CredentialRead` returns `has_oauth: bool`, never
  the token itself. Stream keys are AES-GCM encrypted; only `stream_manager`
  decrypts them, and only in-memory while provisioning a MediaMTX path.
- **Single writer on state.** `stream_manager` is the only module that mutates
  `Stream.state` and the only module that talks to MediaMTX. Routes delegate.
- **JSONB opacity.** `scenes.config`, `chat_components.{config,placement,triggers}`,
  `stream_metrics.raw` are opaque to the DB. Adding a new source or component
  type never requires a migration.
- **Blueprint references.** `chat_components.blueprint_ref` is a string key into
  the Blue blueprint engine (`ZabLaboratory/Blue`). Orion doesn't know the
  component's shape — Blue renders it.

## CI/CD

- **Push / PR**: `ci.yml` runs `ruff`, `mypy`, `pytest`, `pip-audit`, TruffleHog
  secret scan, `uv lock --check`, CODEOWNERS presence.
- **Merge on main**: `deploy.yml` rsyncs to the VPS, writes `.env`, rebuilds
  the image, runs `alembic upgrade head`, restarts the container, checks
  `/health` via the internal network, smoke-tests `/ready` via ZabGate.

### Required GitHub Secrets

| Secret | Role |
|---|---|
| `VPS_HOST`, `VPS_USERNAME`, `VPS_SSH_KEY`, `VPS_APP_PATH` | SSH deployment |
| `ORION_PG_PASSWORD` | Postgres password |
| `ORION_ENCRYPTION_KEY` | AES-GCM key (32 bytes, urlsafe-b64) |
| `TWITCH_CLIENT_ID`, `TWITCH_CLIENT_SECRET` | Twitch app credentials |
| `ORION_PUBLIC_BASE_URL` | e.g. `https://orion.cyell.dev` |
| `ORION_PUBLIC_WHIP_BASE` | e.g. `https://orion-media.cyell.dev` |
| `ORION_TWITCH_OAUTH_REDIRECT_URI` | e.g. `https://zablab.cyell.dev/settings/twitch/callback` |

## Resolution criteria

Conforme à `docs/rules/git.md`. Une branche est **résolue après merge** sur `main` quand :

1. **Squash merge effectué** sur `main` par le mainteneur.
2. **Tous les jobs CI verts** sur le commit de merge.
3. **Déploiement sur le VPS** terminé sans rollback, `/health` répond 200 via l'Intranet Docker et via ZabGate.
4. **Migration appliquée** sans erreur (`alembic upgrade head`).
5. **Aucune régression** `.health.json` niveau `critical` ou `high`.
6. **Branche supprimée** du remote après le squash.
