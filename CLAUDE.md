# Orion

@../../docs/rules/git.md
@../../docs/rules/security.md
@../../docs/rules/agents.md
@../agents/_shared/architecture.md
@../agents/_shared/conventions.md
@../agents/_shared/deploy.md
@../agents/_shared/projects.md

## Status — v2 scaffold landed

Orion v2 (Go) scaffold lives under this repo per
**[ADR 004 — Orion v2 (reactive runtime)](../docs/adr/004-orion-v2-runtime.md)**.

The v0.x Python implementation was deleted on 2026-05-02. The v2
scaffold went onto `feature/v2-go-scaffold` on 2026-05-02, all tests
green locally (`go test ./...` + `go test -tags e2e ./...`), awaiting
maintainer push.

## Stack

| Layer | Technology |
|---|---|
| Runtime | Go 1.26.2 — single statically-linked binary |
| HTTP / WS | `net/http` (1.22 routing) + `coder/websocket` |
| DB | `pgx/v5` directly (no ORM); `goose` migrations |
| Logging | `log/slog` |
| Metrics | `prometheus/client_golang` on internal-only endpoint |
| Test | stdlib `testing`; `httptest` + `coder/websocket` test client |

No web framework. Stdlib + small libs.

## Layout

```
Orion/
├── cmd/orion/main.go                    process entry, wires every dep
├── internal/
│   ├── compiler/                        Canvas+Blue+components → graph+bundle
│   ├── runtime/                         show, scene loop, tick, test sessions
│   ├── adapters/                        inbox, http poller, pg-listen
│   ├── ws/                              upgrade, codec, connection
│   ├── api/                             HTTP handlers (one file per resource)
│   ├── store/                           pgx repositories
│   ├── auth/                            ZabGate header parsing + show-token validate
│   ├── obs/                             slog, prom metrics, panic handler
│   ├── protocol/                        ADR 002 envelope + golden fixtures
│   └── config/                          env parsing
├── migrations/0001_init.sql             scenes / definitions / pushed_versions / assets
├── Dockerfile                           multi-stage distroless (orion + goose + migrations)
├── docker-compose.yml                   local-dev compose
├── docker-compose.prod.yml              prod compose (zab-internal, no host ports)
├── tests/e2e/                           build-tagged tests against a live PG
├── .github/workflows/ci.yml             vet / test / build / docker / staticcheck / golangci / trufflehog
├── .github/workflows/deploy.yml         VPS deploy (rsync + compose build + goose migrate + gateway smoke)
├── .env.template                        every env var documented
└── go.mod                               go 1.26.2
```

## Endpoints (ADR 004 § 2)

All routes start at `/api/v1/...` (ZabGate strips its `/orion`
prefix on the way in).

| Method + path | Purpose |
|---|---|
| `GET /api/v1/health` | liveness |
| `GET /api/v1/ready` | readiness (DB ping + scene roster) |
| `POST /api/v1/scenes/{id}/push` | compile + persist + activate |
| `GET /api/v1/scenes/{id}/render-bundle?v={hash}` | Solar fetch |
| `GET /api/v1/scenes/{id}/operator-inputs?v={hash}` | non-Solar surface |
| `GET /api/v1/scenes/{id}/graph?v={hash}` | internal debug |
| `POST /api/v1/scenes/{id}/status` | archive / reactivate |
| `GET /api/v1/show` | show summary |
| `POST /api/v1/show/active-scene` | switch active scene |
| `POST /api/v1/show/test-sessions` | open isolated test session |
| `GET /api/v1/assets/{id}` | content-addressed binary |
| `GET /api/v1/credentials/{id}/stream-key` | preserved verbatim — currently 503 until Quasar wires it |
| WS `/api/v1/show/stream` | live show |
| WS `/api/v1/scenes/{id}/test?session={uuid}` | isolated scene preview |
| `GET /static/solar/v{N.N.N}/*` | static Solar bundle (immutable) |

## Resolution criteria — coverage

The 18 criteria from the chantier brief (15 from ADR 004 § 12 + 3
chantier-specific). Status as of the v2 scaffold landing:

| # | Criterion | Coverage |
|---|---|---|
| 1 | `POST /push` accepts envelope, advances `latest_pushed_version`; malformed leaves it unchanged. | `internal/api/scenes_push.go` + `tests/e2e/push_test.go` |
| 2 | `POST /show/active-scene` rejects scenes never pushed (`SCENE_NOT_PUSHED`). | `internal/api/show.go` |
| 3 | `GET /render-bundle?v=` byte-for-byte match + immutable cache header. | `internal/api/scenes_get.go` |
| 4 | WS `/show/stream` input-to-delta ≤ 50 ms. | `internal/ws/server_test.go::TestWS_OperatorEndToEnd` (200 ms threshold via WS) + `internal/runtime/scene_test.go` (50 ms direct). |
| 5 | Scene switch emits `scene_changed` + `snapshot` ≤ 100 ms. | `internal/runtime/scene_test.go::TestShow_SwitchMigratesLiveSubsAndEmitsSceneChanged` |
| 6 | 5 Hz HTTP poll → leaf writes + deltas. | `internal/adapters/poller_test.go::TestPoller_5HzWritesAtCadence` |
| 7 | Test session WS accepts `__test.*`; live show rejects with `WRITE_FORBIDDEN`. | `internal/ws/server_test.go::TestWS_LiveRejectsTestNamespace` |
| 8 | Pulsar CEF show-token → viewer; cannot send `input`. | `internal/ws/server_test.go::TestWS_ViewerCannotInput` |
| 9 | Re-push of active scene mid-broadcast emits `scene_changed` + fresh `snapshot`. | `internal/api/scenes_push.go` (push handler), runtime tests cover the emit path. |
| 10 | Pushing not-loaded scene makes it live without restart. | `internal/api/scenes_push.go::Show.Load` (idempotent swap). |
| 11 | Restart reseeds from declared defaults (no persisted live state). | `internal/runtime/state.go::State.Seed` + `cmd/orion/main.go::loadActiveScenes`. |
| 12 | Solar bundle served from `/static/solar/v{N}/*` with long-TTL cache. | `internal/api/static.go` |
| 13 | Archive purges compiled artefacts; pointer reset to null. | `internal/api/scenes_get.go::handleArchive` |
| 14 | Archive on active scene rejected with `SCENE_IN_USE`. | `internal/api/scenes_get.go::handleArchive` |
| 15 | `{rollback_to: ...}` re-points without recompile. | `internal/api/scenes_push.go::handleRollback` |
| 16 | Solar mock-orion fixtures round-trip against the real Orion. | `internal/protocol/fixtures_test.go` (golden fixtures byte-stable). |
| 17 | Cyclic component reject (`CYCLIC_COMPONENT`). | `internal/compiler/compile_test.go::TestCompile_CyclicComponent` |
| 18 | Impure compute reject (`IMPURE_COMPUTE`). | `internal/compiler/compile_test.go::TestCompile_ImpureCompute` |

## Resolution criterion (branch-level, per workspace `git.md`)

The branch is resolved when:
1. CI green on `main`.
2. Deploy workflow green on `main` (`docker compose up -d` against
   prod compose succeeds; `GET /orion/api/v1/health` returns 200 via
   ZabGate).
3. Smoke run: `POST /scenes/{id}/push` against a stub Canvas/Blue
   round-trips; `WS /show/stream` accepts a connection.

## Concerns relocated

- **Twitch** (OAuth Helix + IRC chat + EventSub) → **Quasar** (ADR 005).
  No code lifted from the v0.x Python; Quasar implements the surface
  from scratch.
- **Streaming media plane** (RTMP / WHIP via MediaMTX) → **Pulsar**.
  Pulsar pushes RTMP directly to Twitch.

## Concern preserved verbatim

- The stream-key handover endpoint
  `GET /orion/api/v1/credentials/{id}/stream-key` — referenced by
  Prism's `src/main/broadcast-engine.ts`. Returns 503
  (`TWITCH_CREDENTIAL_UNAVAILABLE`) until Quasar wires through.
