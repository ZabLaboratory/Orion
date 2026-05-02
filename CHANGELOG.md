# Changelog

All notable changes to Orion land here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every release section is written *before* the tag is pushed — the
`release.yml` workflow extracts the section matching the tag and uses
it as the GitHub Release body. If a section is missing, the release
publishes with empty notes.

## [Unreleased]

## [1.0.0] - 2026-05-02

First release of the Go rewrite per
[ADR 004 — Orion v2 (reactive runtime)](../docs/adr/004-orion-v2-runtime.md).
The v0.x Python implementation (Twitch orchestrator + MediaMTX relay) is
fully retired ; Twitch lives in **Quasar** going forward (ADR 005),
the streaming media plane lives in **Pulsar** (bundled in Prism), and
Orion now exclusively owns the scene compiler + reactive runtime + WS
fan-out. 18/18 chantier resolution criteria covered by tests, deployed
live via the merged CI/Deploy workflow on `main`.

### Added — v2 Go scaffold

- **Reactive runtime in Go** scaffolded on `feature/v2-go-scaffold`
  on 2026-05-02 per
  [ADR 004 — Orion v2 (reactive runtime)](../docs/adr/004-orion-v2-runtime.md).
  All 18 chantier resolution criteria (15 from ADR § 12 + 3 chantier-
  specific) covered by tests; `go test ./...` and
  `go test -tags e2e ./...` green locally.
- **Scene compiler** (`internal/compiler/`) — fetches Canvas + Blue +
  components, validates types/cycles/purity, hoists `operator_inputs`
  with instance-path prefixing, emits graph + render bundle, hashes
  to a deterministic `scene_version`. Cycle detection rejects with
  `CYCLIC_COMPONENT` (criterion 17); impure compute rejects with
  `IMPURE_COMPUTE` (criterion 18).
- **Reactive engine** (`internal/runtime/`) — per-scene goroutine,
  drain-then-compute event loop, topologically-sorted DAG, dirty
  propagation. Singleton `Show` owns the active-scene authority and
  migrates live subscribers between scenes on switch (no WS reset).
  Process-wide `Tick` source for time-based bindings.
  `TestSessionManager` clones graphs for `__test.*` workflows.
- **Adapters** (`internal/adapters/`) — unified inbox with scope check
  + fan-out routing (every scene that declared a binding on the
  target path), HTTP poller with 429 backoff, PG `LISTEN/NOTIFY`
  primitive.
- **WS server** (`internal/ws/`) — `coder/websocket` upgrade,
  per-connection state, drain-then-write loop, server-driven ping at
  60 s idle, sends snapshots on backpressure collapse.
- **HTTP API** (`internal/api/`) — every endpoint from ADR 004 § 2:
  `/scenes/{id}/push` (compile + rollback), `/render-bundle`,
  `/operator-inputs`, `/graph`, `/scenes/{id}/status`, `/show`,
  `/show/active-scene`, `/show/test-sessions`, `/assets/{id}`, the
  preserved `/credentials/{id}/stream-key` (503 until Quasar lands),
  and `GET /static/solar/v{N.N.N}/*`.
- **Persistence** (`internal/store/`) — `pgx/v5` repositories for
  scenes, definitions (kept forever), pushed versions (purged on
  archive), and assets. Migrations under `migrations/` (goose
  format).
- **Auth** (`internal/auth/`) — trust-headers parser
  (`X-Authenticated-User`/`-Role`/`-Paths`) plus a cached ZabAuth
  `/validate` client for show-token revocation checks.
- **Protocol** (`internal/protocol/`) — typed envelopes + golden
  fixtures (byte-stable for criterion 16 — Solar mock-orion suite
  conformance).
- **Deploy + CI** — multi-stage distroless `Dockerfile`,
  `compose.yaml` with `orion-postgres`, GitHub Actions running vet /
  test (race) / build / docker / staticcheck / golangci-lint /
  trufflehog.

### Removed

- **Entire v0.x Python implementation deleted** on 2026-05-02 per
  [ADR 004 — Orion v2 (reactive runtime)](../docs/adr/004-orion-v2-runtime.md).
  `src/`, `tests/`, `alembic/`, `scripts/`, `deploy/`, `Dockerfile`,
  `docker-compose.yml`, `docker-compose.prod.yml`, `pyproject.toml`,
  `uv.lock`, `Makefile`, `alembic.ini`, `.github/`, `.env.example` are
  all gone. `CHANGELOG.md`, `README.md`, `CLAUDE.md`, `.gitignore`
  remain ; v2 scaffolded into the same project repo on
  `feature/v2-go-scaffold`.
- The Twitch concerns (OAuth Helix, IRC chat plumbing, encrypted
  credentials) move to **Quasar** per
  [ADR 005 — Quasar (multi-platform integrations)](../docs/adr/005-quasar-platforms.md).
  No code is migrated verbatim — Quasar reimplements the Twitch
  surface from scratch. User OAuth tokens do not migrate ; operators
  re-authorize once after Quasar lands.
- The streaming media plane (RTMP/WHIP, MediaMTX integration) is
  retired. **Pulsar** (bundled in Prism) pushes RTMP directly to
  Twitch — Orion is no longer in the media path.

### Note

Orion v0.x was the Twitch orchestrator + browser-composed-scene
relay ; ADR 004 explicitly drops every concern except scene
compilation + reactive runtime. Until v2 ships, the entire `/orion/*`
prefix routes to a non-existent upstream and will return 502 from
ZabGate. Prism's broadcast pre-flight surfaces this as a clear
`twitch_credential` failure.

## [0.4.0] - 2026-04-30

**Architectural pivot.** Orion stops being a streaming control plane and
becomes a pure Twitch orchestrator. The broadcast media path moves to
[Pulsar](https://github.com/ZabLaboratory/Pulsar) (bundled in Prism),
which pushes directly to Twitch RTMP without ever touching Orion.

Orion now owns : Twitch credentials (AES-GCM), OAuth Helix flow, and
the IRC chat plumbing scaffold. EventSub, expanded Helix endpoints, and
the new subscriber-driven IRC supervisor land in follow-up PRs.

### Removed

- **`streams`, `stream_metrics`, `stream_destinations`** tables —
  dropped in migration `0006_drop_streaming`.
- **`chat_messages.stream_id`** — chat is channel-keyed only ; analytics
  group by `channel` and `sent_at`.
- **`stream_state` PostgreSQL enum** — gone with `streams`.
- **MediaMTX integration** — `services/mediamtx.py`,
  `services/stream_manager.py`, `services/stream_events.py`,
  `routes/streams.py`, `routes/destinations.py`, `routes/metrics.py`,
  `routes/mediamtx_auth.py`. Container `orion-mediamtx` and
  `mediamtx/mediamtx.yml` retired.
- **`ChatSupervisor`** — was reconciling live streams ↔ IRC connections.
  A subscriber-driven replacement lands in a follow-up PR.
- **Caddy `orion-media.cyell.dev` snippet** — public WHIP/HLS subdomain
  no longer needed.
- **Env vars** : `MEDIAMTX_API_URL`, `MEDIAMTX_WHIP_BASE`,
  `MEDIAMTX_PUBLIC_WHIP_BASE`, `TWITCH_RTMP_BASE`,
  `INGRESS_TOKEN_TTL_SECONDS`, `PUBLIC_BASE_URL`,
  `ORION_PUBLIC_BASE_URL`, `ORION_PUBLIC_WHIP_BASE`.

### Changed

- **`_schema` catalogue** — narrowed to `chat_messages` only.
  `streams`, `stream_destinations`, `stream_metrics` entries gone.

## [0.3.0] - 2026-04-25

Phase 4 — scene-switcher. Streams gain a curated overlay playlist + a
dedicated endpoint to swap the active overlay live, plus a state
WebSocket that fans the change out to every subscribed client (the
broadcaster, mobile companion, Stream Deck plugin via Companion). The
piece that turns Orion from "single-overlay broadcast pipe" into "OBS-
class scene switcher", with WHIP session preserved across switches.

### Added

- **`streams.overlay_playlist`** — JSONB column carrying the list of
  overlay ids the operator can swap to live. Pre-declared at stream
  creation (or via `PUT /streams/{id}` while not LIVE), so a stale
  macro request can't activate an arbitrary overlay and blank the
  broadcast.
- **`POST /api/v1/streams/{id}/active-overlay`** — focused endpoint
  designed to fire mid-broadcast. Validates that the new overlay id
  is in the stream's playlist (or `None` to clear → raw camera
  fallback). Unlike `PUT /streams/{id}` which refuses while LIVE,
  this one is *the* live-edit path. Persists, commits, then publishes
  an `active_overlay_changed` event.
- **`WS /api/v1/streams/{id}/state`** — live event stream for one
  stream. Forwards every JSON frame published on
  `stream_event_bus`. Today carries `active_overlay_changed` and
  `state_changed` (lifecycle); new event types are additive — clients
  ignore unknown `event` strings.
- **`StreamEventBus`** — in-process pub/sub primitive (mirrors the
  existing `ChatEventBus`). Per-stream queues with overflow drop-
  oldest semantics — slow subscribers can't pin RAM. Replace with
  Redis pub/sub if Orion ever scales beyond one replica.

### Changed

- `start_stream` / `stop_stream` routes now publish `state_changed`
  events on commit so subscribers see lifecycle transitions on the
  same channel as overlay switches — one socket, both feeds.
- `StreamCreate` / `StreamUpdate` / `StreamRead` / `StreamSummary`
  carry `overlay_playlist`. UUID values are serialised to strings on
  disk (JSONB-friendly) and coerced back at the schema layer.
- Reversible alembic migration `0003_playlist` adds the column with
  server default `'[]'::jsonb` so existing rows pick up the new shape
  without a backfill step.

### Notes

- Phase 4 unlocks two follow-ons: the **mobile companion app**
  (Capacitor + the same Orion API) and **macro integrations**
  (Stream Deck plugin or Bitfocus Companion module). All three
  surfaces consume the same operator command set —
  `start`/`stop`/`active-overlay` over HTTP plus the state WS.
- The renderer-side change (broadcaster re-mounts overlay on switch
  while preserving the WHIP MediaStream) ships in Prism v0.12.0.

## [0.2.0] - 2026-04-25

<!-- commits-since: v0.1.0 -->

### Changed (breaking)

- **Architectural pivot — Orion no longer authors scenes.** Visual
  composition lives in ZabCanvas (`/canvas/api/v1/overlays`); blueprint
  components are hydrated by Blue at render time. Orion shrinks down to
  what's intrinsically a streaming concern: credentials, stream
  lifecycle, MediaMTX orchestration, Twitch IRC pump.
  - **Dropped tables**: `scenes`, `chat_components`. Their associated
    routes, services, and models are gone (`/api/v1/scenes/*`,
    `/api/v1/chat/components/*`).
  - **Streams** now point at a ZabCanvas overlay via a soft pointer:
    `streams.scene_id` (FK) → `streams.overlay_id` (UUID, nullable, no
    cross-service FK). Orion never dereferences it — the renderer
    (Prism / ZabView) is the one that fetches and composes.
  - **Migration `0002_pivot`** drops scenes/chat_components, drops and
    recreates streams + chat_messages + stream_metrics. Existing
    streams/scenes/chat data was scaffold and is not preserved.

### Added

- **Streaming parameters on the Stream row** — Orion is now the control
  panel for every encoder knob. New columns drive the ffmpeg transcode:
  `target_width`, `target_height`, `target_fps`, `video_bitrate_kbps`,
  `audio_bitrate_kbps`, `keyframe_interval_s`, `encoder_preset`. Default
  matches Twitch Partner-tier 1080p30 6 Mbps with a 2 s keyframe
  interval.
- **`PUT /api/v1/streams/{id}`** — patch streaming parameters or the
  overlay reference between sessions. Forbidden while LIVE/PREPARING
  (would silently drift from the running ffmpeg child).
- **`build_twitch_relay_config(stream_key, path, params)`** — ffmpeg
  command line is now templated from a `StreamingParams` dataclass
  instead of being hardcoded. Tests covering the rendered command stay
  green.

### Kept

- Twitch credentials (encrypted stream keys + OAuth tokens), MediaMTX
  external auth webhook, OAuth loopback flow, chat IRC supervisor +
  `/api/v1/chat/live/{channel}` WS (Blue blueprints subscribe over WS
  for chat-driven components), `chat_messages` capture for
  replay/audit.

## [0.1.0] - 2026-04-24

### Added

- **First release.** Orion is now live at
  `https://zabgate.cyell.dev/orion/*`, MediaMTX media endpoint at
  `https://orion-media.cyell.dev`, paired client shipped in Prism
  v0.3.1.
- **Scenes.** CRUD canvas scenes (sources: webcam / screen / image /
  text / color / iframe / canvas-overlay, audio mixer). Config is
  opaque JSONB so the editor can grow without migrations.
- **Twitch credentials.** Stream keys + optional OAuth tokens
  AES-GCM-encrypted at rest; only presence flags ever leave the API.
- **Stream lifecycle.** `pending → preparing → live → stopping →
  ended | error`. `stream_manager` is the single writer — the only
  module that mutates state and the only one that talks to MediaMTX.
- **MediaMTX orchestration.** Dynamic path provisioning via the v3
  control API; `runOnReady` spawns ffmpeg on first publish to
  transcode WebRTC VP8/Opus into H264/AAC at 6 Mbps, 2-second
  keyframes (Twitch's hard requirement) and pushes to RTMP.
- **External auth webhook.** MediaMTX calls `/mediamtx/auth` on every
  publish; Orion verifies the single-use ingress token from the WHIP
  query string, flips the stream to LIVE, and rejects the session
  otherwise. Reads stay permissive (paths are unguessable UUIDs).
- **Twitch OAuth.** Two-leg flow with HMAC-signed state envelope.
  Accepts any `http://localhost:*` loopback redirect URI (for the
  Prism desktop flow) or the configured web URI, signs it into state,
  and echoes it byte-for-byte on token exchange (RFC 6749 §4.1.3).
- **Twitch chat prep.** IRC client + `ChatSupervisor` lifespan task
  that reconciles live streams against IRC connections every 10 s,
  pumps messages into `chat_messages` (for replay) and the in-process
  bus (for live WS fan-out).
- **Chat components.** CRUD for blueprint-backed overlays
  (`blueprint_ref` points at Blue). Orion owns placement + triggers,
  Blue owns rendering.
- **Live metrics WS.** `/api/v1/metrics/streams/{id}/live` polls
  MediaMTX every 2 s and streams state transitions + runtime snapshot
  to subscribed clients until the stream ends.

### Infrastructure

- Postgres 16 schema (6 tables + `stream_state` enum) via Alembic.
- Gateway-first: all control-plane traffic routes through
  `zabgate.cyell.dev/orion/*` (JWT validated at the gateway,
  `X-Authenticated-User` injected into every upstream call).
- MediaMTX container pinned to `bluenviron/mediamtx:latest-ffmpeg`
  (plain `latest` ships without ffmpeg).
- Deploy workflow uses SCP + a runner-side rendered `.env` instead of
  an indented heredoc over SSH so the Windows-runner path mangling
  can't leak into the script.
