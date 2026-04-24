# Orion

Streaming control plane for the Zablab platform. Replaces OBS by delegating
scene composition to the browser and relaying the resulting feed to Twitch
through MediaMTX.

See [CLAUDE.md](./CLAUDE.md) for the full data model, lifecycle, conventions,
and deployment expectations.

- **Port**: `4007`
- **Gateway prefix**: `/orion`
- **DB port (local)**: `5447`
- **MediaMTX ports**: `8889` WHIP, `1935` RTMP, `8554` RTSP, `8888` HLS, `9997` API
- **Docker network**: `zab_network` (external)

## Quick start

```bash
# Install
uv sync

# Dev (backend only, port 4007)
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

# Full stack (API + Postgres + MediaMTX)
docker network create zab_network   # one-off, only if missing
docker compose up -d
```

## API

All endpoints live under `/api/v1`. The gateway strips the `/orion` prefix, so
browsers call `/orion/api/v1/...` and Orion receives `/api/v1/...`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Liveness + DB connectivity |
| `GET/POST` | `/api/v1/scenes` | List / create scene |
| `GET/PUT/DELETE` | `/api/v1/scenes/{id}` | Fetch / update / delete |
| `GET/POST` | `/api/v1/credentials` | List / create Twitch credential |
| `PUT/DELETE` | `/api/v1/credentials/{id}` | Update / delete |
| `GET/POST` | `/api/v1/streams` | List / create stream |
| `POST` | `/api/v1/streams/{id}/start` | Provision MediaMTX path, return WHIP URL |
| `POST` | `/api/v1/streams/{id}/stop` | Tear down |
| `POST` | `/api/v1/twitch/oauth/authorize` | Get Twitch authorize URL |
| `POST` | `/api/v1/twitch/oauth/callback` | Exchange code for tokens |
| `GET/POST` | `/api/v1/chat/components` | Blueprint-referenced chat overlays |
| `WS`  | `/api/v1/chat/live/{channel_login}` | Live chat fan-out |
| `GET` | `/api/v1/metrics/streams/{id}` | Historical metrics |
| `WS`  | `/api/v1/metrics/streams/{id}/live` | Live stream state |

Identity comes from the `X-Authenticated-User` header injected by ZabGate
after JWT validation. No local JWT validation on Orion — ZabGate is the only
auth layer.

## Browser flow

1. Admin creates a Scene + Credential (in `Zablab` `/orion`).
2. Admin creates a Stream; Zablab opens `ZabView /broadcast/{streamId}` in a new tab.
3. Broadcaster page fetches the scene, mounts sources (webcam / screen /
   images / text / overlays), composes onto a `<canvas>`, and calls
   `canvas.captureStream()` for video.
4. Click "Go live" → `POST /orion/api/v1/streams/{id}/start` returns a WHIP
   URL. The page posts its SDP offer directly to MediaMTX.
5. MediaMTX accepts the WebRTC publisher and spawns ffmpeg to push the feed
   to `rtmp://live.twitch.tv/app/<decrypted stream key>`.

See `CLAUDE.md` for detailed wiring.
