# Reuse one JSON encoder in canonical graph serialization

Date: 2026-09-27
Base: Orion `main` after PR #450, `b4e3f57cc6261bb064271dc13aa19b873c28f637`
Scope: shared compiler canonical JSON writer; no scene-, product-, or tenant-specific path.

## Change and invariants

`writeCanonical` now reuses one `json.Encoder` for sorted object keys and scalar
values instead of calling `json.Marshal` separately for every key and leaf.
`Encoder.Encode` adds a final newline, which is removed from the shared buffer
immediately. Its default HTML escaping and number/string encoding match the
previous `json.Marshal` path. Object traversal and sorted-key order are
unchanged.

The cross-language canonical-byte fixtures, scene-version goldens, complete Go
suite, and 20k-node scene-version hash all remained unchanged. The repeated 20k
fixture returned
`sha256:119011db23be0a489d006bd5bf8c7cbfbca3f14356b639bd05fac36558675401`
on every baseline and candidate iteration.

## Measurement

Command, five consecutive runs per version, on Windows amd64 / Ryzen 7 3800X:

```text
go test ./internal/runtime -run '^TestPerf20k_CompileAndColdStartGate$' -count=5 -v
```

| Measure | Baseline median | Candidate median | Change |
|---|---:|---:|---:|
| Compile 20,001 nodes | 213.2122 ms | 187.1442 ms | −12.2% |
| Cold start + full evaluation | 44.7992 ms | 42.4513 ms | −5.2% |

Compile samples (ms), baseline: 212.4417, 213.8693, 213.2836, 213.2122,
211.5375. Candidate: 192.2260, 181.8160, 186.4941, 191.9363, 187.1442.
All ten runs passed. The compile improvement is the measured target; the
cold-start change is smaller and descriptive.

The 250-image typed static-preview benchmark remained effectively flat:
median 7.137843 ms before and 7.100944 ms after (−0.5%), 3,628,133 vs
3,628,130 B/op, and 12,826 allocations/op in both. No general speedup is
claimed for that workload.

A ten-run CPU profile of the 20k fixture showed `writeCanonical` at 0.88 s
cumulative before and 0.67 s after. Cumulative profile rows overlap and are
diagnostic only; the five-run wall-clock test above is the primary comparison.

## Integration and regression proof

The actual Prism → local Orion process → built Solar → Chrome 153.0.8010.53
pipeline used the merged Prism and Solar trees. Candidate and baseline control
reports are:

- Candidate: `D:\Documents\Zab\Prism\.worktrees\local-20260927-prism-inline-image-cost\evidence\local-20260927-orion-canonical-encoder-rerun\eleven\20260927T053901Z-run\report.json`
- Baseline control: `D:\Documents\Zab\Prism\.worktrees\local-20260927-prism-inline-image-cost\evidence\local-20260927-orion-canonical-encoder-baseline-control\eleven\20260927T053958Z-run\report.json`

The two successful runs produced ten byte-identical PNGs. The 100-unique-image
probe used one batch request and matched SHA-256
`5567762f68661db1a69da339239a76b5fd63b43e44024a8c4aa4088e0dfc44e6`; the
30-image customer fixture used three requests and matched SHA-256
`92a3448f208eaaff4f5cdbf08c4d597f02fe376c1c176bbc539146b21d448690`. Both
runs had zero page errors and confirmed cleanup of the Orion process they
started. The candidate executable SHA-256 was
`aa81de1b27024d94de2fa7ccf6516f7f7569ef1ffcf0a0147474ad5ab12196c5`.

One earlier candidate run at
`D:\Documents\Zab\Prism\.worktrees\local-20260927-prism-inline-image-cost\evidence\local-20260927-orion-canonical-encoder-premerge\eleven\20260927T053534Z-run\`
failed only the strict live-vs-fresh byte comparison for the distinct-image
probe: decoded pixels differed by at most one RGB level, with no visible layout
or content change. That mismatch did not reproduce in the subsequent candidate
run or the baseline control; both then matched each other byte-for-byte. The
failed run is retained as evidence rather than suppressed. No assertion was
weakened.

## Validation

- `go test ./... -count=1`
- `go vet ./...`
- `go build ./...`
- `staticcheck ./...`
- `golangci-lint run --timeout=5m`
- `python scripts/check_file_sizes.py --self-test`
- `python scripts/check_file_sizes.py`
- Prism preview-pipeline typecheck and the two successful real integrated runs above

## Rollback

Revert the single compiler change to `writeCanonical` in
`internal/compiler/compile_graph_helpers.go`; no data migration or wire-format
change is involved.
