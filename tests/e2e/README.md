# tests/e2e

End-to-end tests that exercise the full Orion stack against a live
Postgres + stubbed upstream services (Canvas, Blue, ZabAuth).

The tests are built with the `e2e` build tag so the default `go test
./...` invocation does not hit a database.

## Run locally

```sh
docker compose -f deploy/compose.yaml up -d orion-postgres
ORION_E2E_DATABASE_URL=postgres://orion:CHANGEME@localhost:5447/orion?sslmode=disable \
  go test -tags e2e ./tests/e2e/...
```

## What's covered

| Criterion | Scenario |
|---|---|
| 1 | `POST /push` happy path advances `latest_pushed_version`; malformed body leaves it unchanged. |
| 2 | `POST /show/active-scene` rejects a scene without a pushed version (`SCENE_NOT_PUSHED`). |
| 3 | `GET /render-bundle?v={hash}` matches the compiler's bytes byte-for-byte; immutable cache header set. |
| 9 | Re-push of the active scene emits `scene_changed` + fresh `snapshot` to subscribers without WS reset. |
| 10 | Pushing a scene that wasn't previously loaded makes it selectable as the active scene immediately. |
| 11 | Process restart reseeds every scene from declared defaults; no persisted live state. |
| 13 | Archiving a non-active scene purges its compiled artefacts; `latest_pushed_version` resets to null. |
| 14 | Archiving the active scene rejected with `SCENE_IN_USE`. |
| 15 | `POST /push` with `{rollback_to: ...}` re-points the pointer without recompilation. |
