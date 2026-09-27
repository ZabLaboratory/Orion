# Orion static LSML node-field decoding candidate

Date: 2026-09-27 11:42 UTC
Work unit: `local-20260927-orion-node-fields`
Base: `origin/main` at `9e876a4581ee10865632db333726363936d18464` (PR #455 merge)

## Change

Decode each static child node directly into its structural fields and final
`LayoutNode.Props` map instead of first allocating a temporary
`map[string]json.RawMessage` for every child. The existing map adapter remains
the compatibility path for unsupported scanner forms. No product-, scene-, or
Pulsar-specific behavior was added.

## Performance evidence

Command on Windows/amd64, AMD Ryzen 7 3800X, Go 1.26.4:

```text
go test ./internal/compiler -run '^$' -bench '^BenchmarkCompileStaticLSMLRepresentative$' -benchmem -benchtime=2s -count=5
```

The clean `origin/main` worktree and candidate worktree were measured serially
with the same command. Medians of five samples:

| Revision | Time/op | Bytes/op | Allocs/op |
| --- | ---: | ---: | ---: |
| `9e876a4581ee10865632db333726363936d18464` | 2,275,828 ns | 828,834 B | 9,811 |
| candidate | 2,199,612 ns | 728,528 B | 9,311 |
| Change | -3.35% | -12.1% | -5.1% |

This is a local microbenchmark, not an end-to-end latency claim. Timing samples
vary; allocation and byte reductions are the more stable result.

## Correctness and integration evidence

- Every Orion CI job requests the `zab-orion` repository label. This public
  repository is served by repo-scoped JIT runners; the label opts into the
  orchestrator's faster queued-job rediscovery instead of relying only on its
  slower backstop poll. This changes dispatch only, not test coverage or code
  execution semantics.

- `go test ./... -count=1`: pass.
- `go vet ./...`: pass.
- `staticcheck ./...`: pass.
- `golangci-lint run --timeout=5m`: pass.
- `python scripts/check_file_sizes.py`: 465 text files, four pre-existing
  reviewed exceptions, no growth.
- Scanner/map parity tests cover duplicate keys, escaped keys, duplicate
  children, ignored non-object children, and invalid UTF-8 property keys; a
  five-second fuzz run completed without a mismatch.
- Prism Preview-pipeline E2E on candidate binary SHA-256
  `1d9e8b3dff523e788051b5ced5d4dba6afc71133cc511e837731677adc335c9c`:
  `D:\Documents\Zab\Prism\.worktrees\local-20260927-prism-inline-image-cost\evidence\local-20260927-orion-node-fields-candidate\eleven\20260927T113658Z-run\report.json`.
- Compared with exact-main control binary SHA-256
  `b3f2c47229ea2c3eb3bc339baf2ddb8b6db6562a71b7ba9675c1cb83881716d1` and
  report `D:\Documents\Zab\Prism\.worktrees\local-20260927-prism-inline-image-cost\evidence\local-20260927-orion-main-postmerge-after-nodeparser-rerun\eleven\20260927T112208Z-run\report.json`:
  all 10 PNGs are byte-identical; Solar and Prism source hashes match; fresh,
  converged, distinct-source, and customer-scene checks pass; no errors; owned
  Orion stopped.

The #455 post-merge control had one initial fresh-capture comparison with a
one-channel (`+1`) difference while its live screenshot SHA remained stable.
The same binary passed on immediate rerun, and all ten outputs then matched the
candidate and prior #454 control exactly. This is retained as harness/capture
variability evidence, not concealed as a clean first attempt.

## Scope and remaining proof

The E2E exercises Prism -> Orion -> Solar; it is not a production deployment,
Pulsar validation, or an end-to-end runtime-latency measurement. After merge,
repeat the E2E using a binary built from the exact `origin/main` tree and compare
all ten PNGs against this candidate.
