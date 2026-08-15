//go:build e2e

// Package e2e holds Orion's build-tagged, cross-service end-to-end tests
// (`go test -tags e2e ./tests/e2e/...`, CI job `e2e (Postgres)`).
//
// History (#15, #331, ADR-BLUE-012 §4.3): every test that used to live here
// drove the FULL legacy stack against a live Postgres — push/validate/
// active-scene, the SQLite embedded-local store, service-token rotation, PG
// advisory locks — none of which exist anymore. The shared harness
// (requireDB/startOrion in the removed push_test.go) was used package-wide,
// so retiring the store retired the whole suite at once, not file by file.
// This file was, until work unit ORION-E2E-GATE-CONTRACT-27, a bare
// placeholder whose only job was to keep `go test -tags e2e` compiling
// ("ok, no tests to run") — a green gate asserting nothing.
//
// Current coverage: canvas_layout_contract_test.go exercises the ZabCanvas
// -> Orion CanvasLayout contract (issue #27) through the real
// compiler.HTTPFetcher + compiler.Compile path against a ZabCanvas-shaped
// fixture. It needs no database — see that file's doc comment.
//
// Still NOT covered by any e2e harness: the stateless-cutover path
// (scene-intent, attestation verification, bluehost). It has unit coverage
// (internal/attestation, internal/workload, internal/bluehost, internal/api)
// but no live end-to-end proof — see the #331 final report for the
// coverage-loss accounting this package has not yet closed.
package e2e
