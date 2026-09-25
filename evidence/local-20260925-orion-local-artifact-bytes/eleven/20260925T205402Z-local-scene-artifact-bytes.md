# Orion embedded-local artifact path — 2026-09-25

## Change

Embedded-local scene activation now carries the bytes read from the content-addressed artifact store directly to the common digest-verification path. It no longer base64-encodes those bytes into a JSON envelope and parses/decodes them again. Inline requests now populate the same in-memory envelope directly; fetched workload responses keep the existing JSON/base64 wire representation.

The artifact reader preallocates from the already-validated file size, reads exactly that many bytes, and rejects a file that grows or shrinks during the read. The existing 1 GiB bound and program, LSML, and render-bundle digest checks remain in force.

## Benchmark

`BenchmarkLoadLocalSceneEnvelope` uses the same temporary files before and after the change: a 1 MiB Blue program, a 512 KiB LSML bundle, and a 2 MiB render bundle (3.5 MiB total). It measures local file loading and envelope/representation construction; it does not measure the full scene-intent handler or a Blue/Pulsar transition.

| Revision | ns/op samples | B/op samples | allocs/op samples |
| --- | --- | --- | --- |
| Before | 21,437,413; 18,190,749; 18,110,095 | 35,828,276; 35,564,402; 35,985,402 | 135; 134; 134 |
| After | 1,660,422; 2,299,583; 2,235,373 | 3,677,220; 3,677,232; 3,677,222 | 44; 44; 44 |

Across these runs, median loader time fell from 18.19 ms to 2.24 ms, allocated bytes from 35.8 MB to 3.68 MB, and allocations from 134 to 44. Treat this as a focused local-loader benchmark, not an end-to-end activation claim.

## Validation

- `go test ./internal/api -run 'TestLoadLocalSceneEnvelopeKeepsArtifactBytes|TestPostLocalAtomicSceneIntent' -count=1` — passed.
- `go test ./...` — passed across all Orion packages.
- `go vet ./...`, `go build ./...`, `go build -tags e2e ./...`, `staticcheck ./...`, and `golangci-lint run --timeout=5m` — passed.
- `git diff --check` — passed.
- `go test -race -count=1 ./...` could not run locally: this Windows Go environment has `CGO_ENABLED=0` and no C compiler. The repository CI race job remains necessary.

## Static LSML compiler fast path

`rewriteJSON` now preserves valid JSON fragments unchanged when they contain no backslash escapes and no literal `assets/` reference. Escaped fragments and possible asset references still use the existing decode/rewrite/encode path. This avoids generic map/slice materialization for the common untouched case while retaining the same asset rewrite behavior. Tests cover large integer preservation, escaped slash/prefix spellings of asset references, and invalid JSON rejection.

The before/after measurements use the same synthetic 250-node static scene and benchmark fixture on this Windows host. A CPU profile of the pre-change fixture attributed 22.00% cumulative time to `rewriteJSON`, with `encoding/json.Unmarshal` at 42.86% and `encoding/json.Marshal` at 14.78%; cumulative rows overlap and should not be added together.

| Benchmark | Before (median) | After (median) | Change |
| --- | ---: | ---: | ---: |
| `rewriteJSON` untouched | 11.158 µs, 2,177 B, 63 allocs | 0.647 µs, 0 B, 0 allocs | 94.2% lower time |
| `CompileStaticLSML` representative | 11.616 ms, 2,992,220 B, 45,156 allocs | 6.440 ms, 2,212,691 B, 26,622 allocs | 44.6% lower time; 26.0% fewer bytes; 41.0% fewer allocs |

This is a local microbenchmark on a constructed scene, not a production-scene corpus or an end-to-end scene activation measurement. Full-suite CI remains required, particularly the repository's self-hosted race job.

Validation after the fast path: `go test ./internal/compiler -count=1`, `go test ./...`, `go vet ./...`, `go build ./...`, `staticcheck ./...`, `golangci-lint run --timeout=5m`, and `git diff --check` all passed locally.
