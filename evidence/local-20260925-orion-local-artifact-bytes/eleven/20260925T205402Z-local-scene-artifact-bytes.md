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
