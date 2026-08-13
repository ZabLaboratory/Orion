# tests/e2e

RETIRED (#15, #331, ADR-BLUE-012 §4.3). Every test here drove the legacy
Postgres/SQLite-store stack — `POST /push`, `POST /show/active-scene`,
archive/reactivate, service-token rotation — none of which exist in Orion
anymore (stateless cutover, no store).

`e2e_test.go` is a placeholder (`//go:build e2e`, empty package) so
`go test -tags e2e ./tests/e2e/...` still succeeds; it provides no coverage.

No live-Postgres end-to-end harness exists yet for the stateless path
(scene-intent → ZabCanvas attestation → bluehost). Coverage for that path
today is unit-level: `internal/attestation`, `internal/workload`,
`internal/bluehost`, `internal/api` (scene-intent handlers). Standing up a
new e2e harness against the new model is follow-up work, not part of #331.

`testdata/` is kept — its fixtures (e.g. the Canvas/Blue scene-bundle JSON)
are harmless and may still be useful to a future harness.
