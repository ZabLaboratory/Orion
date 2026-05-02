# Orion

@../../docs/rules/git.md
@../../docs/rules/security.md
@../../docs/rules/agents.md
@../agents/_shared/architecture.md
@../agents/_shared/conventions.md
@../agents/_shared/deploy.md
@../agents/_shared/projects.md

## Status — v1 deleted, v2 scaffold pending

The Python + FastAPI Orion (v0.x) was deleted on 2026-05-02 per
**[ADR 004 — Orion v2 (reactive runtime)](../docs/adr/004-orion-v2-runtime.md)**.

This directory is intentionally near-empty. v2 (Go) scaffolds in here
in a follow-up PR. The repository's `.git/` is preserved so v2 lands
on the same project history.

## Scope, once v2 lands

Orion v2 is a **reactive runtime** for the platform's compiled scenes :
scene compiler (Canvas + Blue + components → graph + Solar render
bundle), per-scene goroutine event loop, WS fan-out to live show
subscribers (Solar / Prism / mPrism / Companion / Quasar), test
sessions, and Postgres-backed pushed-version persistence.

Concerns moved out :

- **Twitch** (OAuth Helix + IRC chat + future EventSub) → **Quasar**
  (ADR 005). The previous `twitch_credentials` + `chat_messages`
  tables and the Helix OAuth flow are reimplemented from scratch in
  Quasar. User tokens do **not** migrate — operators re-authorize
  once after Quasar rolls out.
- **Streaming media plane** (RTMP/WHIP via MediaMTX) → **Pulsar**
  (bundled in Prism). Pulsar pushes RTMP directly to Twitch ; Orion
  v2 never touches the media path.

Concern preserved verbatim :

- The stream-key handover endpoint
  `GET /orion/api/v1/credentials/{id}/stream-key` — referenced by
  Prism's main process (`src/main/broadcast-engine.ts`). v2's Go
  implementation re-exposes it under the same path. Until v2 ships,
  any Prism broadcast attempt will fail the pre-flight
  `twitch_credential` check with a clear error — that's intentional.

## Stack (v2, decided in ADR 004 — not yet implemented here)

| Layer | Technology |
|---|---|
| Runtime | Go 1.23+ — single statically-linked binary |
| HTTP / WS | `net/http` (1.22 routing) + `coder/websocket` |
| DB | `pgx/v5` directly (no ORM), `goose` migrations |
| Logging | `log/slog` (stdlib) |
| Metrics | `prometheus/client_golang` on internal-only endpoint |
| Test | stdlib `testing` + `testify/assert` |

No web framework. Stdlib + small libs.

## What's still here

- `CHANGELOG.md` — preserved with the deletion entry on top, full
  v0.x history below for archaeology.
- `README.md` — short status note pointing at this file and ADR 004.
- `.gitignore` — preserved as-is for v2.
- `.git/` — same project repo, v2 commits land on `main` after the
  rewrite branches.

Anything else (`src/`, `tests/`, `alembic/`, `scripts/`, `deploy/`,
`Dockerfile`, `pyproject.toml`, `uv.lock`, `docker-compose.yml`,
`docker-compose.prod.yml`, `Makefile`, `.github/`, `.env.example`)
was deleted and will be reintroduced by the v2 scaffold.

## Resolution criteria — N/A until v2 lands

The deletion branch is resolved when the maintainer squash-merges it.
Resolution criteria for v2 ship inside ADR 004 § 12 and rewrite this
section.
