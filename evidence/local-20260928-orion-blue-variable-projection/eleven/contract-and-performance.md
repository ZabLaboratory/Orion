# Orion–Blue variable-projection contract

## Change

On Orion's BlueWire adapter, `Step` and `Tick` now use additive projected
Blue runtime APIs. `bluehost.Host` requests only the cumulative
`__show.emit` and `__overlay-app.set` bags it dispatches. An additive
`WritePlatformEventProjected` host method uses the same projection for other
Orion callers. Existing full-snapshot host/runtime methods remain available
and keep their previous behavior. The bridge still forwards only the same
runtime sequence and outputs; no BlueWire or LSDP protocol fields changed.

`TestHostProjectedStepTickAndPlatformEventKeepLocalSideEffects` exercises
`StepProjected`, `TickProjected`, and `WritePlatformEventProjected` with an
Execute-mode program. It proves cumulative effect records survive projection,
`show.emit` reaches its sink, `overlay-app.set` reaches the mirror, and
unchanged cumulative records remain deduplicated.

## Validation

- `go test ./...` — pass for all Orion packages.
- `go vet ./...` — pass.
- Blue `runtime/go`: `go test ./...` and `go vet ./...` — pass; its complete
  source tree matches the merged Blue `origin/main` runtime directory.
- Orion chat and scene-admission benchmarks — pass; the chat path produced one
  wire delta per operation.
- `git diff --check` — pass in Orion. Blue has no whitespace defects; Git emits
  a CRLF-to-LF warning for the refreshed runtime manifest in this Windows
  checkout.
- `python scripts/check_file_sizes.py` — pass after splitting static contract
  parsing/type validation into `internal/bluehost/program_metadata.go`;
  `host.go` is now 778 lines, under the existing 1,000-line gate.
- `go test -race` was attempted for Blue and the Orion host/bridge/API packages,
  but Go refused because `CGO_ENABLED=0`; no C compiler is installed here.

The initial Orion validation used the temporary `blue-local.work` in this
worktree:

```go
go 1.26.2

use .

replace github.com/ZabLaboratory/Blue/runtime/go => ../../../Blue/runtime/go
```

Orion has since passed `go test -mod=readonly ./...` and
`go vet -mod=readonly ./...` with `GOWORK=off` and the merged Blue
pseudo-version in go.mod; no local replacement was active. The first CI run on
head `a11b6a3df791c1b74ed933f13ac8d739f7de48aa` failed because
`internal/bluehost/host.go` had 1,024 lines. The refactor extracts existing
metadata/contract functions without changing their logic or lowering the gate;
the local size guard and complete tests now pass. On the next head,
`218b91fd2c2ce2556ecdb374a0e2bca52958d2f0`, `file-sizes` passed but
`build-test` and `staticcheck` failed during their private-module preparation
step. A local `go mod tidy -diff` reproduced a go.sum mismatch from two stale
checksums for the prior Blue version; `go mod tidy` removed only those entries,
`go mod tidy -diff` is now clean, and full tests/vet pass. The fresh CI rerun
for this correction is pending. The projection code is in signed commit
`5d8d785863ce10695af945eba86de278684992dd`; the dependency pin and final
evidence are in the follow-up commit. No Orion deployment was performed.

## Final matched benchmark evidence

Environment: Windows/amd64, Go 1.26.4, AMD Ryzen 7 3800X. Both candidates
used identical fixtures and `-benchmem -benchtime=3s -count=3 -cpu=1`.
Baseline: clean Orion `origin/main`, using the corrected Blue runtime source.
Candidate: this branch with the real merged Blue module pinned in go.mod as
`v0.1.1-0.20260928141015-4cad89dba613`. The Blue `runtime/go` source tree in
the local checkout was byte-for-byte at `origin/main` when the baseline was
measured, so runtime behavior and dependency code match. Metrics are medians;
raw samples follow.

Baseline command, from the clean Orion main checkout (using its temporary
workspace to point at the byte-identical merged Blue runtime source):

```powershell
$env:GOWORK='C:\Users\Mathias\AppData\Local\Temp\codex-zab\01a0b4c9-a426-7450-8ba2-1d64f13e85d5\orion-blue-baseline.work'
go test -mod=readonly github.com/ZabLaboratory/Orion/internal/api -run '^$' -bench '^BenchmarkOrion181Blue(ChatToProjection|SceneAdmission)$' -benchmem -benchtime=3s -count=3 -cpu=1
```

The temporary baseline workspace contained `go 1.26.2`, `use
D:/Documents/Zab/Orion`, and `replace
github.com/ZabLaboratory/Blue/runtime/go => D:/Documents/Zab/Blue/runtime/go`.
It is recorded here for reproducibility; the temporary file itself is removed
after validation.

Candidate command, from this worktree, with the actual merged module and no
workspace override:

```powershell
$env:GOWORK='off'
go test -mod=readonly ./internal/api -run '^$' -bench '^BenchmarkOrion181Blue(ChatToProjection|SceneAdmission)$' -benchmem -benchtime=3s -count=3 -cpu=1
```

| Benchmark | Baseline median | Candidate median | Change |
| --- | ---: | ---: | ---: |
| `BenchmarkOrion181BlueChatToProjection` time | 40,604 ns/op | 37,536 ns/op | −7.6% |
| Chat allocations | 16,577 B/op; 162 allocs/op | 14,513 B/op; 149 allocs/op | −12.5% bytes; −8.0% allocations |
| `BenchmarkOrion181BlueSceneAdmission` time | 713,314 ns/op | 699,173 ns/op | −2.0%; inconclusive |
| Admission allocations | 208,592 B/op; 3,709 allocs/op | 208,592 B/op; 3,709 allocs/op | unchanged |

Chat samples (time, bytes/op, allocs/op):

| Run | Baseline | Candidate |
| ---: | --- | --- |
| 1 | 38,762 ns; 16,574 B; 162 | 37,536 ns; 14,517 B; 149 |
| 2 | 40,865 ns; 16,577 B; 162 | 38,529 ns; 14,503 B; 149 |
| 3 | 40,604 ns; 16,618 B; 162 | 36,251 ns; 14,513 B; 149 |

Admission samples (time, bytes/op, allocs/op):

| Run | Baseline | Candidate |
| ---: | --- | --- |
| 1 | 694,863 ns; 208,591 B; 3,709 | 669,389 ns; 208,592 B; 3,709 |
| 2 | 720,463 ns; 208,592 B; 3,709 | 699,173 ns; 208,591 B; 3,709 |
| 3 | 713,314 ns; 208,592 B; 3,709 | 710,402 ns; 208,592 B; 3,709 |

Chat timing ranges do not overlap in this longer rerun (baseline 38,762–40,865;
candidate 36,251–38,529 ns/op). The change therefore reduces measured chat
latency by 7.6% in this run, alongside 13 fewer allocations and about 2.1 KB
less allocation per operation; the earlier 1-second runs were noisier and are
superseded. Scene admission is outside the modified path: its samples overlap
(baseline 694,863–720,463; candidate 669,389–710,402 ns/op), with identical
allocation count and bytes, so no admission latency gain or regression is
claimed. Each chat operation continued to emit exactly one wire delta.

## Cross-repository merge and deployment evidence

- Blue PR #600 merged by squash as `4cad89dba613ccb53e97678624894da7d91ddd0f`;
  GitHub reported the merge signature as verified and the merge commit is an
  ancestor of `origin/main`.
- Blue CI run `36432067615` completed successfully on head
  `032a44474d065f857dff55a06682cd6100586ed8` (all 9 checks, including pytest).
- Automatic Blue deployment run `36433956825` completed successfully on the
  merge SHA. Its logs report `Blue healthy`, `bluemcp healthy`, database
  migration completion, and gateway `/ready` HTTP 200. Docker emitted a
  non-fatal warning that the existing `blue_pg_data` volume was not created by
  Compose; the workflow did not delete or replace that data volume.
- Orion's code commit is `5d8d785863ce10695af945eba86de278684992dd`; this PR
  adds the verified Blue dependency pin, final evidence, and the file-size
  extraction plus tidy go.sum. A fresh Orion CI and merge are still separate
  required gates.
