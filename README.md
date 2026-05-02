# Orion

Reactive runtime for the Zablab broadcast platform's compiled scenes.
Owns the scene compiler (Canvas + Blue + components → graph + Solar
render bundle), per-scene goroutine event loops, and the WS fan-out
to live show subscribers (Solar, Prism, mPrism, Companion, Quasar).

- **Port** : `4007` (HTTP + WS)
- **Internal port** : `4017` (Prometheus scrape, dev-network only)
- **Gateway prefix** : `/orion`
- **DB port (local)** : `5447`
- **Docker network** : `zab_network` (external)
- **Status** : v2 scaffold landed (Go) — see
  [CLAUDE.md](./CLAUDE.md) for a full layout map and resolution
  matrix; [ADR 004](../docs/adr/004-orion-v2-runtime.md) for the
  spec.

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
