# Orion

> **v1 deleted on 2026-05-02.** Awaiting the v2 Go rewrite per
> [ADR 004 — Orion v2 (reactive runtime)](../docs/adr/004-orion-v2-runtime.md).
> See [CLAUDE.md](./CLAUDE.md) for what survived, what moved, and
> what's next.

## What Orion v2 will be (once scaffolded)

Reactive runtime for the platform's compiled scenes. Owns the scene
compiler (Canvas + Blue + components → graph + Solar render bundle),
per-scene goroutine event loops, and the WS fan-out to live show
subscribers (Solar, Prism, mPrism, Companion, Quasar).

- **Port** : `4007`
- **Gateway prefix** : `/orion`
- **DB port (local)** : `5447`
- **Docker network** : `zab_network` (external)

## What Orion is **no longer** responsible for

- **Twitch** (OAuth Helix + IRC chat + EventSub) → moved to **Quasar**
  (ADR 005). The previous Twitch credentials, OAuth flow, and chat
  plumbing are reimplemented from scratch in Quasar.
- **Streaming media plane** (RTMP/WHIP via MediaMTX) → moved to
  **Pulsar** (bundled in Prism). Pulsar pushes RTMP directly to
  Twitch.

## What's preserved across the rewrite

The stream-key handover endpoint
`GET /orion/api/v1/credentials/{id}/stream-key` — referenced by
Prism's main process (`src/main/broadcast-engine.ts`). v2's Go
implementation re-exposes it under the same path. Until v2 ships,
Prism's pre-flight `twitch_credential` check fails clearly.

## History

The v0.x Python codebase shipped a streaming control plane around
MediaMTX, browser-composed scenes relayed to Twitch, and a Twitch
orchestrator (OAuth + IRC chat prep). The v0.4.0 release pivoted away
from streaming control ; ADR 004 then ruled out a refactor in favour
of a Go rewrite, and ADR 005 split off Twitch into its own service.
The full v0.x changelog stays in [CHANGELOG.md](./CHANGELOG.md).
