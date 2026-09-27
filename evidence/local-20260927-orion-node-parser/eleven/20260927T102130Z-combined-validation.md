# Orion combined compiler optimization validation

Work unit: `local-20260927-orion-node-parser`

## Integrated revision

Orion PR #454 (generic size-object fast path) merged as GitHub-verified squash
`ca1b5f9fc961c2fba7f9cc2cd0c6218e567dfa0e`. All ten required CI checks passed.
The parser candidate was rebased onto that exact `origin/main` revision. Its
code commit is `51c66b7a62b144ca011c06c0126d4f07fe7dd918`, tree
`c1dd733b044c6c06ea662558948f7bfb2721af56`. This is the same source tree used
for the combined local checks and Prism integration run below; the code commit
is signed. The existing parser-only benchmark remains in
`20260927T095623Z-performance.md`.

## Combined compiler benchmark

Machine: Windows/amd64, AMD Ryzen 7 3800X. Fixture:
`BenchmarkCompileStaticLSMLRepresentative` (250 text nodes). The combined
candidate and #454-only control use the same command:

```text
go test ./internal/compiler -run '^$' -bench '^BenchmarkCompileStaticLSMLRepresentative$' -benchmem -benchtime=2s -count=5
```

| Revision | Median time | Median B/op | Median allocs/op |
| --- | ---: | ---: | ---: |
| Post-#454 main (size fast path) | 3.500 ms | 941,425 | 11,337 |
| Post-#454 + static-node scanner | 2.148 ms | 829,716 | 9,811 |

The node scanner adds 38.6% lower median compile time, 11.9% fewer allocated
bytes, and 1,526 fewer allocations/op (13.5%) relative to #454 alone. These
are local compiler microbenchmark results, not production latency claims.

## Correctness checks

On this exact combined source tree, all passed:

- `go test ./... -count=1`
- `go vet ./...`
- `staticcheck ./...`
- `golangci-lint run --timeout=5m`
- `python scripts/check_file_sizes.py` (463 text files, 4 reviewed exceptions,
  no growth)
- `git diff --check`

The candidate also retains the parser parity/fuzz tests documented in the
parser-only report. The signed rebase did not change the tested source tree.

## Prism → Orion → Solar regression proof

The control binary was built from exact post-merge `origin/main` at #454 commit
`ca1b5f9`; its SHA-256 is
`89c4b7dd87c4cd3ce45c633f0e71d611a4f12b0def80a7eb19268d4b100652d4`. The
rebased combined candidate binary SHA-256 is
`3fecc7fa29767a8646f5f5a76940959cfda703cc0ce50991f42c897b3072930e`.

Control report:

`Prism/.worktrees/local-20260927-prism-inline-image-cost/evidence/local-20260927-orion-main-postmerge-after-split/eleven/20260927T101600Z-run/report.json`

Candidate report:

`Prism/.worktrees/local-20260927-prism-inline-image-cost/evidence/local-20260927-orion-node-parser-postmerge/eleven/20260927T101858Z-run/report.json`

The candidate's 10 PNG files are byte-identical to the exact-main control.
Solar file hashes and Prism source hashes also match. Fresh/converged image
assertions plus the 100-distinct-source and customer-scene probes passed;
`errors` is empty and the owned Orion process stopped.

Scope: real Prism gateway/scene server/asset cache → local Orion process/HTTP/WS
→ built Solar in Chrome. This is not Pulsar/Twitch or physical first-pixel
coverage. Original scene asset binaries were unavailable and replaced with
deterministic PNGs; no Blue graph executes in this harness.
