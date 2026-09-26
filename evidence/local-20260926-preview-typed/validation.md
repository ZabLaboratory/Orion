# Orion — typed editable Preview compilation

## Result

The production wiring now supplies an in-process RenderBundle compiler to
editable Preview. It shares the lowering implementation with CompileStaticLSML,
but avoids marshaling the whole bundle and immediately unmarshaling it. The
persisted byte API, static scene-intent path and render-bundle validation endpoint
still use the same bytes/digest path. Byte-only integrations retain their fallback.

Defaults remain independently owned by the immutable bundle and mutable scene
seeds, including each RawMessage slice. The precompiled-root path still decodes
when an object is needed and returns the original bytes verbatim to byte callers.

## Validation

- `go test ./...`: passed, all packages (including compiler, API, runtime,
  Blue host/wire, LSDP, authentication, attestation and artifact contracts).
- `go vet ./...`: passed.
- New equivalence cases cover assets, large integers, editable bindings,
  transitions, profiles, defaults, raw precompiled byte identity and errors.
- HTTP tests exercise both typed and byte compilers: successful activation,
  compile failure 422, invalid compiled root 500, patch acceptance and stale
  sequence rejection 409. They also prove the typed path does not call the byte
  compiler and fails closed on a nil bundle.
- `git diff --check`: passed.
- `go test -race ./internal/compiler ./internal/api ./internal/runtime` could
  not run: this Windows environment has CGO disabled and no GCC/Clang toolchain
  found. This is an explicit validation gap, not a passing race test.

## Measurement

Command: `go test ./internal/compiler -run '^$' -bench BenchmarkStaticPreviewCompilation -benchmem -count=5`.
Raw results: [benchmark.txt](benchmark.txt). Medians on Windows/amd64, Ryzen 7
3800X; synthetic authoring trees with 4 KiB encoded-image strings in defaults.
No image decode, HTTP activation, final storage serialization or paint is timed.

| Images | Old compile + JSON decode | Typed compile | Allocated bytes, old → typed |
| --- | ---: | ---: | ---: |
| 25 | 1.979 ms | 0.958 ms | 589,396 → 394,314 |
| 250 | 18.519 ms | 9.132 ms | 8,438,641 → 3,932,319 |

For 250 images: approximately 50.7% less CPU time and 53.4% fewer allocated bytes
in this phase. The benchmark compares the byte API + decode and typed API in the
same candidate; the byte API uses the unchanged lowering logic.

## Integration / delivery boundary

Base: `cd2fea40fd9471a9e11153025e82909783945a14`.
Branch: `codex/orion-preview-typed-compile`; changes remain local/uncommitted.
Prism successfully built this source into its worktree's Orion sidecar:

- Source digest: `sha256:ecf967750141429efca234db9ba8701a88d0019c41062a63ae936dd975cb20d3`.
- Executable SHA-256: `66fc7b5f2a6f3aebccf7ca67246a1df548e3e4a103ac7d12740ac89d6998f982`.
- Blue source: `6b57b3570762c56745068f282ee6cf21cdf8e84e`, unchanged by this task.

No push, merge, deployment or physical broadcast test. Pulsar was not modified
or launched. Rollback: omit the typed dependency from production wiring; the
existing byte fallback remains tested. No data migration is involved.
