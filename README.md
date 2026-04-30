# Orion

Twitch orchestrator for the Zablab platform. Owns Twitch credentials (encrypted at rest), the OAuth Helix flow, and IRC chat plumbing.

It does **not** own broadcast media. Streaming is handled externally by [Pulsar](https://github.com/ZabLaboratory/Pulsar) (broadcast engine bundled in Prism), which pushes directly to Twitch RTMP. Pulsar is an external module — Orion never touches it, and Pulsar never touches Orion's infrastructure.

See [CLAUDE.md](./CLAUDE.md) for the full data model, conventions, and deployment expectations.

- **Port**: `4007`
- **Gateway prefix**: `/orion`
- **DB port (local)**: `5447`
- **Docker network**: `zab_network` (external)

## Quick start

```bash
# Install
uv sync

# Dev (port 4007)
uv run uvicorn orion.main:app --reload --port 4007

# Env
cp .env.example ../.env.orion
# Fill ENCRYPTION_KEY and Twitch credentials.

# Lint / typecheck / test
uv run ruff check .
uv run mypy src
uv run pytest

# Migrations
uv run alembic upgrade head

# Full stack (API + Postgres)
docker network create zab_network   # one-off, only if missing
docker compose up -d
```

## API

All endpoints live under `/api/v1`. The gateway strips the `/orion` prefix, so browsers call `/orion/api/v1/...` and Orion receives `/api/v1/...`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Liveness + DB connectivity |
| `GET/POST` | `/api/v1/credentials` | List / create Twitch credential |
| `PUT/DELETE` | `/api/v1/credentials/{id}` | Update / delete |
| `POST` | `/api/v1/twitch/oauth/authorize` | Get Twitch authorize URL |
| `POST` | `/api/v1/twitch/oauth/callback` | Exchange code for tokens |
| `WS` | `/api/v1/chat/live/{channel_login}` | Live chat fan-out (subscriber-driven supervisor lands in a follow-up PR) |
| `GET` | `/api/v1/_schema` | QueryMe catalogue (read-only, blueprint-facing) |
| `POST` | `/api/v1/_query` | QueryMe execution (read-only) |

Identity comes from the `X-Authenticated-User` header injected by ZabGate after JWT validation. No local JWT validation on Orion — ZabGate is the only auth layer.

## Future scope (separate PRs)

- **EventSub webhooks** — subs / donations / bits / follows / raids / hype train.
- **Expanded Helix endpoints** — channel info, schedule, clips, predictions.
- **Subscriber-driven IRC supervisor** — re-introduces chat capture when a WS client subscribes to `/api/v1/chat/live/{channel}`.
