//go:build e2e

// Package e2e is a retirement placeholder (#15, #331, ADR-BLUE-012 §4.3).
//
// Every test previously here drove the FULL legacy stack against a live
// Postgres — push/validate/active-scene, the SQLite embedded-local store,
// service-token rotation, PG advisory locks — none of which exist anymore.
// The shared harness (requireDB/startOrion in the removed push_test.go) was
// used package-wide, so retiring the store retired the whole suite at once,
// not file by file.
//
// This file exists solely so `go test -tags e2e ./tests/e2e/...` still
// succeeds ("ok, no tests to run") rather than failing to compile — it is
// NOT coverage. The stateless-cutover path (scene-intent, attestation
// verification, bluehost) is covered by internal/attestation, internal/
// workload, internal/bluehost and internal/api's unit tests instead; no
// live-Postgres end-to-end harness exists yet for that path. See the #331
// final report for the coverage-loss accounting.
package e2e
