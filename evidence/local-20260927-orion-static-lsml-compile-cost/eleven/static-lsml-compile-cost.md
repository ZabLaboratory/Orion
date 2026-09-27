# Orion static LSML compile cost

Date: 2026-09-27
Base: `origin/main` at `b8c0a5c69c4fc33045a86ba5cc61575b347a62a4`
Scope: shared Orion compiler; no product-, scene-, or customer-specific path.

## Profile signal and changes

A CPU profile of `BenchmarkCompileStaticLSMLRepresentative` (250 text nodes,
Windows amd64, Ryzen 7 3800X) showed repeated JSON decoding in
`adaptStaticNode` and per-node binding-map construction in `lowerText` as
avoidable work. Cumulative profile rows overlap; they are diagnostic, not
additive.

- Decode well-formed child arrays directly as `[]map[string]json.RawMessage`,
  avoiding the intermediate `[]json.RawMessage` plus a second object decode.
  Malformed arrays keep the old tolerant path so valid object siblings survive
  scalar entries; `TestCompileStaticLSMLKeepsValidChildrenAroundMalformedEntries`
  pins that behavior.
- Skip JSON decoding for absent optional node fields.
- Build the text binding rename table once, rather than allocating and filling
  the same map for every text node.

## Benchmarks

Before and after were measured on the same Windows host with:

```text
go test ./internal/compiler -run '^$' -bench 'Benchmark(CompileStaticLSMLRepresentative|StaticPreviewCompilation)$' -benchmem -benchtime=1s -count=3
```

Values below are medians of the three reported runs. These are synthetic
compiler fixtures, not claims about a production scene corpus.

| Benchmark | Before | After | Change |
|---|---:|---:|---:|
| `CompileStaticLSMLRepresentative` time | 5.980 ms | 4.714 ms | −21.2% |
| `CompileStaticLSMLRepresentative` bytes/op | 2,209,436 | 1,292,389 | −41.5% |
| `CompileStaticLSMLRepresentative` allocs/op | 26,621 | 17,347 | −34.8% |
| `StaticPreviewCompilation/images250/typedtrue` time | 10.240 ms | 9.230 ms | −9.9% |
| `StaticPreviewCompilation/images250/typedtrue` bytes/op | 3,932,186 | 3,630,864 | −7.7% |
| `StaticPreviewCompilation/images250/typedtrue` allocs/op | 17,346 | 12,830 | −26.0% |
| `StaticPreviewCompilation/images250/typedfalse` time | 19.942 ms | 18.996 ms | −4.7% |

The non-typed path is also faster in this sample, while its serialized output
contract remains covered by `TestStaticRenderBundleMatchesSerializedPath`.

## Runtime regression control

The Prism → local Orion → Solar test ran once with a baseline Orion binary built
from `main` and once with a binary built from this candidate, using the same
merged Prism/Solar trees and the same fixture. Both reports are in the Prism
worktree:

- Baseline: `D:\Documents\Zab\Prism\.worktrees\local-20260927-prism-inline-image-cost\evidence\local-20260927-prism-inline-image-cost\eleven\comparison-orion-main\20260927T033910Z-run\report.json`
- Candidate: `D:\Documents\Zab\Prism\.worktrees\local-20260927-prism-inline-image-cost\evidence\local-20260927-prism-inline-image-cost\eleven\candidate-orion-static-lsml-compile\20260927T033633Z-run\report.json`

| Signal | `main` Orion | Candidate Orion |
|---|---:|---:|
| Orion boot | 214.5 ms | 214.6 ms |
| 100-image readiness | 722.6 ms | 723.0 ms |
| 30-image customer-fixture readiness | 342.9 ms | 307.1 ms |

Both runs produced byte-identical fresh and converged 100-image PNGs
(`5567762f68661db1a69da339239a76b5fd63b43e44024a8c4aa4088e0dfc44e6`) and
the same customer-fixture PNG
(`92a3448f208eaaff4f5cdbf08c4d597f02fe376c1c176bbc539146b21d448690`). Each
used one batch request for the 100 distinct assets (305,565 bytes); there were
no page errors and both owned Orion processes stopped. The one-run readiness
figures are a regression control, not a stable latency claim.

## Validation

- `go test ./internal/compiler -count=1`
- `go test ./... -count=1`
- `go test ./internal/runtime -run '^TestPerf20k_' -count=1 -v`: 20,001-node
  compile ~207–210 ms, full cold start 44.7 ms, single-input delta p95 2.74 ms
  (200 samples; 999-node cone).
- `go vet ./...`, `go build ./...`, `staticcheck ./...`,
  `golangci-lint run --timeout=5m`
- `python scripts/check_file_sizes.py --self-test` and
  `python scripts/check_file_sizes.py`: passed; no ceiling changes.
- `git diff --check`

The CI PostgreSQL E2E and race jobs remain authoritative; this Windows checkout
has no `ORION_E2E_DATABASE_URL` and `CGO_ENABLED=0`, so neither was claimed as a
local validation.

## Follow-up profile: avoid revalidating trusted JSON fragments

A follow-up CPU profile of `BenchmarkStaticPreviewCompilation/images250/typedtrue`
showed `encoding/json.checkValid` at 31.2% flat CPU. `compileStaticLSML` has
already validated the complete source document with `json.Unmarshal`, but the
unchanged large default fragments were then scanned again by `rewriteJSON`.
The compiler now uses a separate fast path only for `RawMessage` fragments
extracted from that validated source; the defensive `rewriteJSON` entry point
still validates standalone input. Escaped asset references still take the
existing decode/rewrite path, and tests cover both entry points plus rejection
of invalid standalone JSON.

The isolated benchmark used the same Windows host and fixture on both commits:

```text
go test ./internal/compiler -run '^$' -bench '^BenchmarkStaticPreviewCompilation/images250/typedtrue$' -benchmem -benchtime=3s -count=3
```

| Measure | First optimization (`652fd84`) | Candidate with follow-up | Change |
|---|---:|---:|---:|
| Time/op (median) | 9.715 ms | 8.096 ms | −16.7% |
| Bytes/op (median) | 3,630,880 | 3,628,144 | −0.08% |
| Allocs/op | 12,830 | 12,826 | −0.03% |

For context, the same 3-second benchmark on `main` measured 11.656 ms,
3,932,278 bytes/op and 17,346 allocs/op. The final candidate is therefore
30.5% faster with 7.7% fewer bytes and 26.1% fewer allocations in this synthetic
inline-image fixture versus `main`; this is not a claim about every scene.

The final candidate was then exercised through the real local Prism → Orion →
Solar preview harness (same merged Prism/Solar trees, browser, and fixture as the
baseline). Candidate report:

`D:\Documents\Zab\Prism\.worktrees\local-20260927-prism-inline-image-cost\evidence\local-20260926-preview-pipeline\eleven\20260927T041142Z-run\report.json`

Its Orion binary SHA-256 is
`5c1cbd987e3dadb9166ff2e11e302a0385beca02b94c90ba516babeeeb711de0`. Ten PNG
files—25- and 100-image fresh/patched/converged outputs, the 100-distinct-source
fresh/live outputs, and the customer-fixture fresh/live outputs—were compared
byte-for-byte against the `main` Orion baseline. All matched; the browser error
list was empty and the owned Orion process stopped. The 25/100-image output
hashes were respectively
`b87820c9bc6b2f2c46e6500747cd67f9def03a4a865ae69502d8edb68541371f` and
`7bf28e734e0cc63086d312024a526f9bba64b70541f8afd7e5593e548fa4eb3c`; the
distinct-source and customer-fixture hashes also matched the baseline. Runtime
readiness timings remain descriptive single-run measurements only.

After the follow-up change, these local checks passed: `go test ./... -count=1`,
`go vet ./...`, `go build ./...`, `staticcheck ./...`,
`golangci-lint run --timeout=5m`, and both file-size guard commands. The preview
pipeline typecheck and end-to-end run also passed. Remote CI results are tracked
separately in the PR and remain required before merge.
