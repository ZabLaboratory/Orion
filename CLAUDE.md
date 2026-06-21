# Orion

@../../docs/rules/git.md
@../../docs/rules/security.md
@../../docs/rules/agents.md
@../agents/_shared/architecture.md
@../agents/_shared/conventions.md
@../agents/_shared/deploy.md
@../agents/_shared/projects.md
@../agents/_shared/live-testing.md

## Status — production (205 PRs on main)

Orion v2 (Go) is in production on `main`. The v0.x Python implementation
was deleted on 2026-05-02; the Go rewrite shipped progressively through
205 merged PRs. ADR 004 (Orion v2 runtime) and ADR 005 (Quasar concerns
relocation) were superseded or absorbed during the build campaign — their
slot numbers (004, 005) are no longer present in `docs/adr/`; the
numbering jumps 003 → 006 (ADR 006 = exec-activation R9-lift). This is
not a gap: ADR 004/005 content was folded into ADR 003 §3.x and the
Quasar CLAUDE.md respectively before the slot files were removed.

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

## Endpoints

All routes start at `/api/v1/...` (ZabGate strips its `/orion`
prefix on the way in). Source: `internal/api/public.go`.

| Method + path | Purpose |
|---|---|
| `GET /api/v1/health` | liveness |
| `GET /api/v1/ready` | readiness (DB ping + scene roster) |
| `POST /api/v1/scenes/{id}/push` | compile + persist + activate |
| `GET /api/v1/scenes/{id}/render-bundle?v={hash}` | Solar fetch (content-hashed) |
| `GET /api/v1/scenes/{id}/lsml-bundle?v={hash}` | LSML bundle for authoring tools (ADR 002) |
| `GET /api/v1/scenes/{id}/operator-inputs?v={hash}` | non-Solar surface |
| `GET /api/v1/scenes/{id}/graph?v={hash}` | internal debug |
| `POST /api/v1/scenes/{id}/status` | archive / reactivate |
| `POST /api/v1/scenes/{id}/validate` | validate + gate antenna-eligibility (ADR 003 §3.2.2) |
| `GET /api/v1/scenes/{id}/validation` | read last validation record |
| `POST /api/v1/scenes/{id}/exec/completion` | exec completion probe (ADR 006) |
| `POST /api/v1/validate/simulate` | service-scoped simulate (ADR 015, Amendment 1) |
| `GET /api/v1/show` | show summary |
| `POST /api/v1/show/active-scene` | switch active scene |
| `POST /api/v1/show/test-sessions` | open isolated test session |
| `POST /api/v1/show/stream-rules` | create stream-level Blue rule (ADR 009) |
| `DELETE /api/v1/show/stream-rules/{id}` | delete stream-level rule |
| `GET /api/v1/assets/{id}` | content-addressed binary |
| `GET /api/v1/credentials/{id}/stream-key` | preserved verbatim — currently 503 until Quasar wires it |
| WS `/api/v1/show/stream` | live show (legacy) |
| WS `/api/v1/show/stream.lsdp` | LSDP wire — `ORION_LSDP_MODE=dual` required; subprotocol `lsdp.v1.1` |
| WS `/api/v1/scenes/{id}/test` | isolated scene preview (`?session={uuid}`) |
| `GET /static/solar/v{N.N.N}/*` | static Solar bundle (immutable, long TTL) |
| `POST /api/v1/operator/call/{blueprint_id}/{entrypoint}` | fire `core.operator.on-call@1` spine (active-only, 409 if dormant) — Blue ADR 008 §3.2. `{blueprint_id}` = the scene-local blueprint key; the DEFAULT (legacy single / blueprint-free) key is addressed with the token `_` (the empty key cannot ride a path segment). Same token the cockpit announces; `_` is reserved as an authored key (ADR 016 RC-6). |
| `POST /api/v1/operator/resolve/{blueprint_id}/{await_name}` | resolve `core.operator.await-value@1` (type-checked; 410 if scene inactive) — Blue ADR 008 §3.3 |
| `GET /api/v1/runtime/{blueprint_id}/pending` | list armed await-value points — Blue ADR 008 §3.3 |
| `GET /api/v1/cockpit/contracts?stream_id={id}` | aggregate operator-UI contract (params/triggers/awaits, scope scene|stream-level) — Blue ADR 008 §3.5 |
| `GET /api/v1/db/{service}/schema` | introspectable DB catalogue for datasource selectors — Blue ADR 008 §3.4 |

## Resolution criteria — coverage

The 18 criteria from the chantier brief (15 from ADR 004 § 12 + 3
chantier-specific). Status updated post-#88 (conformance matrix merged,
`c145e49`) and post-ADR 006 (exec activation + IMPURE_COMPUTE retired):

| # | Criterion | Coverage |
|---|---|---|
| 1 | `POST /push` accepts envelope, advances `latest_pushed_version`; malformed leaves it unchanged. **ADR 003 #1 (master conformance) — COVERED** (#88 `c145e49`): CI job `conformance-matrix` asserts every manifest node has a registered executor + passing test; ratchet armed. | `internal/api/scenes_push.go` + `tests/e2e/push_test.go` + `internal/conformance/` (job `conformance-matrix`) |
| 2 | `POST /show/active-scene` rejects scenes never pushed (`SCENE_NOT_PUSHED`). **ADR 003 #2 (no capability rejection) — COVERED** (#88 `c145e49`): `TestConformance_Matrix` pushes a blueprint with one node of each served type; `conformance-matrix` CI job gates it. | `internal/api/show.go` + `internal/conformance/conformance_test.go::TestConformance_Matrix` |
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
| 18 | Impure compute reject (`IMPURE_COMPUTE`). **SUPERSEDED par ADR 006** : la partition exec-compilateur tue ce reject ; purity devient scheduling metadata. Les deux tests `TestCompile_ImpureCompute` sont réécrits en assertions de partition (ADR 006 §3.2 + §6 #3). | ~~`internal/compiler/compile_test.go::TestCompile_ImpureCompute`~~ → voir ADR 006 §3.2 |

## ADR ledger — post-ADR 006 decisions (accepted, on main)

| ADR | Title | Status | Key fact |
|---|---|---|---|
| 007 | `core.db.*` atomics as pure descriptor builders | accepted | PR #142 · `compute_db.go` |
| 008 | Active-scene-only execution (dormant roster) | accepted + Amendment 1 | PR #152 (#163 amendment) · inbox routing, freeze-resume |
| 009 | Stream-level Blue rules | accepted | PR #156 #157 · stream-rule union routing + `core.show.emit@1` |
| 010 | `core.http.request@1` canonical executor + content hardening | accepted | PR #160 · query/headers/response_headers/timeout_ms + host-only logging |
| 011 | `core.animation.play@1` keyframe lowering (ADR 003 §3.4 reconciliation) | accepted | PR #161 #164 #167 · scalar gen leaf, compiler lowering, I7 live proof |
| 012 | `core.source.read@1` reclassified to pure compute (Option B) | accepted | PR #165 #166 · compile-time resolution, `__resolved_source` in Config; Blue `is_pure` flip pending |
| 013 | Platform-event exec entrypoint (`core.event.on-platform-event@1`) | accepted | PR #170 #171 #173 #175 · exec entrypoint arming `__events` leaf, reactive payload binding, stream-level rule finale |
| 014 | Blueprint-reference support via compile-time subgraph expansion | accepted | PR #183 #184 #185 #189 #190 · expand at compile, cyclic detection (`CYCLIC_BLUEPRINT_REFERENCE`), `__vars` input-reader promotion |
| 015 | Service-scoped simulate endpoint | accepted + Amendment 1 (2026-06-16) | PR #198 #200 #201 · `POST /validate/simulate`; Amendment 1 = compile Blue graph in-body (not pre-compiled) |
| Blue 008 | Operator-UI contract (runtime surface — Orion side) | accepted | PR #209 #210 #211 #213 #215 · operator call/resolve/pending routes, cockpit aggregate, DB catalog — see Blue ADR 008 |

> Full resolution criteria live in each ADR doc (`docs/adr/`).
> ADR 010 and ADR 012 doc files were committed with this resync (scribe/solar-v029-adr-sync).
> ADR 013/014/015 added by resync 2026-06-20 (drift report).
> Blue ADR 008 (operator-UI contract) added 2026-06-21 : Orion runtime surface delivered in #209/#210/#211/#213/#215.

## Solar version history

| Version | Status | Key change |
|---|---|---|
| v0.2.8 | installed, kept | URL/auth/value fixes (go-live Solar) |
| v0.2.9 | **current go-live** | box `position:absolute;inset:0` + `translateX→x`/`translateY→y` framer-motion (animation fix, ADR 011 I7) |

`SOLAR_VERSIONS: "v0.2.8 v0.2.9"` in `ci.yml` (PR #168). Go-live URL: `static/solar/v0.2.9/host/index.html`.
Rollback: re-point browser-source to `v0.2.8` (already on VPS, instant). Runbook: `docs/runbooks/solar-v029-animation-fix.md`.

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
