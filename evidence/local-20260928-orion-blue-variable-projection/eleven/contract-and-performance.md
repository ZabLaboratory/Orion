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
- `go test ./...` and `go vet ./...` — pass for the Blue `runtime/go` module.
- Orion chat and scene-admission benchmarks — pass; the chat path produced one
  wire delta per operation.
- `git diff --check` — pass in Orion. Blue has no whitespace defects; Git emits
  a CRLF-to-LF warning for the refreshed runtime manifest in this Windows
  checkout.
- `go test -race` was attempted for Blue and the Orion host/bridge/API packages,
  but Go refused because `CGO_ENABLED=0`; no C compiler is installed here.

Orion's go.mod still pins an older Blue runtime. For local verification only,
the removed temporary `blue-local.work` contained:

```go
go 1.26.2

use .

replace github.com/ZabLaboratory/Blue/runtime/go => ../../../Blue/runtime/go
```

Thus this validates both source trees together, but not yet a standalone Orion
build against a published dependency. Blue must first expose these additive
methods in a released/merged revision; Orion's dependency can then move to that
revision and its normal CI can run. No commit, push, merge, or deployment was
performed.

## Benchmark evidence

Environment: Windows/amd64, Go 1.26.4, AMD Ryzen 7 3800X. Both candidates were
measured with `-benchtime=1s -count=5 -cpu=1` against the same benchmark
fixtures. Before/after metrics are medians; raw samples follow.

| Benchmark | Before median | After median | Change |
| --- | ---: | ---: | ---: |
| `BenchmarkOrion181BlueChatToProjection` time | 36,259 ns/op | 34,657 ns/op | −4.4% |
| Chat allocations | 16,226 B/op; 159 allocs/op | 14,189 B/op; 146 allocs/op | −12.6% bytes; −8.2% allocations |
| `BenchmarkOrion181BlueSceneAdmission` time | 674,416 ns/op | 712,367 ns/op | +5.6% median; noisy |
| Admission allocations | 208,592 B/op; 3,709 allocs/op | 208,591 B/op; 3,709 allocs/op | unchanged |

Chat samples (time, bytes/op, allocs/op):

| Run | Before | After |
| ---: | --- | --- |
| 1 | 43,211 ns; 16,226 B; 159 | 34,495 ns; 14,202 B; 146 |
| 2 | 36,259 ns; 16,127 B; 159 | 34,657 ns; 14,185 B; 146 |
| 3 | 35,162 ns; 16,251 B; 159 | 37,393 ns; 14,201 B; 146 |
| 4 | 47,182 ns; 16,225 B; 159 | 34,492 ns; 14,189 B; 146 |
| 5 | 34,438 ns; 16,270 B; 159 | 35,222 ns; 14,156 B; 146 |

Admission samples (time, bytes/op, allocs/op):

| Run | Before | After |
| ---: | --- | --- |
| 1 | 671,140 ns; 208,592 B; 3,709 | 611,597 ns; 208,591 B; 3,709 |
| 2 | 646,998 ns; 208,591 B; 3,709 | 647,778 ns; 208,592 B; 3,709 |
| 3 | 704,291 ns; 208,592 B; 3,709 | 726,289 ns; 208,591 B; 3,709 |
| 4 | 675,846 ns; 208,592 B; 3,709 | 719,183 ns; 208,592 B; 3,709 |
| 5 | 674,416 ns; 208,592 B; 3,709 | 712,367 ns; 208,591 B; 3,709 |

Admission samples vary broadly and overlap (before 646,998–704,291 ns/op;
after 611,597–726,289 ns/op). Bytes and allocations are unchanged, and this
optimization does not touch admission. Treat the latency median movement as
noise, not a measured regression or gain.
