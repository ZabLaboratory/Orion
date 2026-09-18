# Orion

[![Release](https://img.shields.io/github/v/release/ZabLaboratory/Orion?logo=github)](https://github.com/ZabLaboratory/Orion/releases/latest)
[![CI](https://github.com/ZabLaboratory/Orion/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ZabLaboratory/Orion/actions/workflows/ci.yml)
[![Local runtime release](https://img.shields.io/badge/Prism-local%20runtime-0f766e)](https://github.com/ZabLaboratory/Orion/releases/latest)

Reactive runtime for the Zablab broadcast platform's compiled scenes.
Owns the scene compiler (Canvas + Blue + components → graph + Solar
render bundle), per-scene goroutine event loops, and the WS fan-out
to live show subscribers (Solar, Prism, mPrism, Companion, Quasar).

- **Port** : `127.0.0.1:4007` (HTTP + WS)
- **Internal port** : `127.0.0.1:4017` (local diagnostics only)
- **Local base path** : `/orion` (loopback only; no remote gateway deployment)
- **Status** : v2 local runtime — the same Go runtime is built as the
  Prism sidecar; the former remote Orion service and gateway deployment are
  retired. See
  [CLAUDE.md](./CLAUDE.md) for the layout map and resolution matrix;
  [ADR 004](../docs/adr/004-orion-v2-runtime.md) for the contract.

## Prism local runtime

Each Orion release tag publishes platform binaries and
`prism-orion-runtime-manifest.json`. The manifest includes the SHA-256 digest
of every platform artifact and the Blue runtime `source_digest` embedded at
build time. Prism downloads the matching local binary during its authenticated
startup gate, verifies the digest, and activates it only after Blue confirms
the same pair. Scene switching and show-slot execution then stay on the local
Orion process; ZabCanvas, ZabTruth, ZabRanking and Quasar remain gateway
services for synchronization and live data. No release is deployed as a public
Orion endpoint.

## Quick start

```sh
# Install deps + verify
go mod tidy
go vet ./...
go test ./...

# Build the binary
go build -o ./bin/orion ./cmd/orion

# Boot the local Prism sidecar against a loopback gateway fixture
ORION_PROFILE=embedded-local \
ORION_LOCAL_OPERATOR_SECRET=dev-only-change-me \
ORION_SQLITE_PATH=./.orion/orion.sqlite \
ORION_ZABAUTH_VALIDATE_URL=http://127.0.0.1:4000/auth/api/v1/tokens \
ORION_CANVAS_BASE_URL=http://127.0.0.1:4000/canvas \
ORION_BLUE_BASE_URL=http://127.0.0.1:4000/blue \
ORION_ZABGATE_URL=http://127.0.0.1:4000 \
./bin/orion
```

## Repository layout

See [CLAUDE.md](./CLAUDE.md) — full tree plus the spec → file mapping
for every chantier resolution criterion.

## Concerns relocated

- **Twitch** (OAuth Helix + IRC chat + EventSub) → **Quasar** (ADR 005).
- **Streaming media plane** (RTMP/WHIP via MediaMTX) → **Pulsar**
  (bundled in Prism).

## Concern preserved verbatim

- `GET /orion/api/v1/credentials/{id}/stream-key` — exposed on the local
  loopback runtime and referenced by
  Prism's main process. Returns 503 until Quasar wires the
  underlying Twitch credential storage.
