# tests/e2e

Build-tagged (`//go:build e2e`) cross-service tests. Run by CI job
`e2e (Postgres)`: `go test -v -tags e2e -count=1 -timeout 120s ./tests/e2e/...`.

## Current coverage

- **`canvas_layout_contract_test.go`** — ZabCanvas -> Orion `CanvasLayout`
  contract (issue #27, ADR-002 §6 C-min acceptance #1, work unit
  `ORION-E2E-GATE-CONTRACT-27`). Serves `testdata/canvas_layout_from_canvas.json`
  — byte-identical to the fixture ZabCanvas's own `test_emit_go_contract_fixture`
  generates from the real `layout_adapter.adapt_bundle_to_layout` — over an
  `httptest.Server`, and drives it through the real `compiler.HTTPFetcher` +
  `compiler.Compile` (fetch, decode, expand, lower, emit). Proves the wire
  shape AND that a bound leaf path (e.g. `title.bindings.value`) survives
  compile unchanged, not just that the JSON parses.
  **No database.** The contract is HTTP + JSON decode + an in-memory compile;
  it runs in this job because this job is what `tests/e2e/...` maps to, not
  because it needs `ORION_E2E_DATABASE_URL`.
  If the ZabCanvas-side fixture generator (`tests/test_layouts.py`) or the
  producer adapter changes shape, re-copy
  `ZabCanvas/tests/contract/canvas_layout_from_canvas.json` over this repo's
  copy — this test does not regenerate it, it only decodes it.

## History (#15, #331, ADR-BLUE-012 §4.3)

Every test previously in this package drove the legacy Postgres/SQLite-store
stack — `POST /push`, `POST /show/active-scene`, archive/reactivate,
service-token rotation — none of which exist in Orion anymore (stateless
cutover, no store). `e2e_test.go` was a bare placeholder from #331 until this
issue; it now carries the package doc instead.

## Still not covered

No live end-to-end harness exists yet for the stateless-cutover path
(scene-intent → ZabCanvas attestation → bluehost). Coverage for that path
today is unit-level: `internal/attestation`, `internal/workload`,
`internal/bluehost`, `internal/api` (scene-intent handlers). Standing up an
e2e harness against that model — which, unlike the CanvasLayout contract,
may genuinely need Postgres for attestation/workload state — is follow-up
work, not covered by #27.

`testdata/` fixtures beyond `canvas_layout_from_canvas.json` (e.g. the
Canvas/Blue scene-bundle JSON) are kept — harmless, may still be useful to
that future harness.
