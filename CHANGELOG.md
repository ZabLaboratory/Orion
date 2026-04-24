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
