# Orion candidate and toolchain gate

- Candidate source: `29afefa038487dd45e9f7ec03b1ade591ce54532`, the clean worktree configured for Prism development. This source contains the #463 lineage and subsequent native reconnect fixes.
- `go test ./...`: passed locally on Windows with Go 1.26.4. This tests code behavior but does not validate the release toolchain.
- The earlier #463 `govulncheck` job used Go 1.26.6 and reported reachable standard-library vulnerabilities fixed in Go 1.26.9. The CI and release workflows now pin Go 1.26.9; [Go's release history](https://go.dev/doc/devel/release#go1.26.9) dates the patch to 2026-10-08.
- Both workflow YAML files parsed, and `git diff --check` passed.

The updated CI, packaged sidecar provenance, and Prism clean-profile Studio startup remain release gates. No published Orion binary is claimed by this record.
