## Spike — `github.com/ZabLaboratory/Blue/runtime/go` consumable from Orion

Isolated spike proving the Blue runtime Go module is importable and runnable
from Orion (Load→Start→Step→Stop, `internal/bluespike/spike_test.go`).
`go build/vet/test ./...` green locally. Not wired into `cmd/orion`.

### CI: private module auth

The module is a private cross-repo import, so CI needs a scoped Git
credential before `go mod`/`go build` can clone it. This reuses
`BLUE_READ_APP_ID`/`BLUE_READ_APP_PRIVATE_KEY` — the same App already live in
`ZabCanvas/.github/workflows/integration.yml:59-66` for the identical need
(read-only cross-repo access to `Blue`). No new App, no new secret surface.

**Not yet provisioned on Orion.** `BLUE_READ_APP_ID`/`BLUE_READ_APP_PRIVATE_KEY`
are set as repo secrets on ZabCanvas; they are not yet set on `Orion`. CI on
this PR will fail at the "mint Blue-read App token" step until they're added
here (same App, no new key, no rotation — just extend its secret scope to
this repo). Provisioning needs a named clearance (Bastion) before being
applied, per bail `B3-R6-10-ORION-HOST-SPIKE`.

**Credential handling (Bastion review, 2026-08-13):** the first draft of this
PR left the App token live in `~/.gitconfig` for the whole job — readable by
any later step that runs PR-submitted code (`go test`/`golangci-lint`/
`go install`), and `insteadOf` rewrote every `https://github.com/` clone, not
just `ZabLaboratory/`. Both fixed: each job now mints the token, writes an
`insteadOf` rewrite scoped strictly to `https://github.com/ZabLaboratory/`,
runs `go mod download` (+`go mod tidy` check in the `build` job), then unsets
the credential — all inside one step, before any later step touches PR code.
An alternative (`go mod vendor`, no credential in CI at all) was evaluated
and dropped: Go vendoring is all-or-nothing across the dependency graph, so
it would have committed ~213 MB (mostly `modernc.org/sqlite`) for a spike
that only needs a few KB from `Blue/runtime/go` — irreversible repo bloat for
a bad trade.

C4 (App 4496939 registration/permissions confirmation) and C5/C6 (registry
sync, multi-site key rotation) remain open — tracked outside this spike, not
blocking.

Refs #331
