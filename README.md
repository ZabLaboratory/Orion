# Orion

[![Release](https://img.shields.io/github/v/release/ZabLaboratory/Orion?logo=github)](https://github.com/ZabLaboratory/Orion/releases/latest)
[![CI](https://github.com/ZabLaboratory/Orion/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ZabLaboratory/Orion/actions/workflows/ci.yml)
[![Local runtime release](https://img.shields.io/badge/Prism-local%20runtime-0f766e)](https://github.com/ZabLaboratory/Orion/releases/latest)

Reactive runtime for the Zablab broadcast platform's compiled scenes.
Owns the scene compiler (Canvas + Blue + components → graph + Solar
render bundle), per-scene goroutine event loops, and the WS fan-out
to live show subscribers (Solar, Prism, mPrism, Companion, Quasar).

- **Port** : `4007` (HTTP + WS)
- **Internal port** : `4017` (Prometheus scrape, dev-network only)
- **Gateway prefix** : `/orion`
- **DB port (local)** : `5447`
- **Docker network** : `zab_network` (external)
- **Status** : v2 production runtime — the same Go runtime is built as a
  local Prism sidecar and as the deployable Orion service. See
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
services for synchronization and live data.

## Quick start

```sh
# Install deps + verify
go mod tidy
go vet ./...
go test ./...

# Build the binary
go build -o ./bin/orion ./cmd/orion

# Boot against the dev DB
docker compose up -d orion-postgres
ORION_DATABASE_URL=postgres://orion:CHANGEME@localhost:5447/orion?sslmode=disable \
ORION_ZABAUTH_VALIDATE_URL=http://zabgate:4000/auth/api/v1/tokens \
ORION_CANVAS_BASE_URL=http://zabgate:4000/canvas \
ORION_BLUE_BASE_URL=http://zabgate:4000/blue \
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

- `GET /orion/api/v1/credentials/{id}/stream-key` — referenced by
  Prism's main process. Returns 503 until Quasar wires the
  underlying Twitch credential storage.
