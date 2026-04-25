# Changelog

All notable changes to Orion (the Zablab streaming control plane) land
here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every release section is written *before* the tag is pushed — the
`release.yml` workflow extracts the section matching the tag and uses it
as the GitHub Release body (which the Discord webhook then picks up).
If a section is missing, the release publishes with empty notes.

## [Unreleased]

_Nothing staged yet._

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
