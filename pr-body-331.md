## Spike — `github.com/ZabLaboratory/Blue/runtime/go` consumable from Orion

Isolated spike proving the Blue runtime Go module is importable and runnable
from Orion (Load→Start→Step→Stop, `internal/bluespike/spike_test.go`).
`go build/vet/test ./...` green locally. Not wired into `cmd/orion`.

### CI: private module auth

The module is a private cross-repo import, so every CI job needs a scoped
Git credential before `go mod`/`go build` can clone it. Instead of minting a
new `FORGE_GH_APP_ID`/`FORGE_GH_APP_PRIVATE_KEY` App as originally drafted,
this PR reuses `BLUE_READ_APP_ID`/`BLUE_READ_APP_PRIVATE_KEY` — the same
short-lived-token pattern already live in
`ZabCanvas/.github/workflows/integration.yml:59-66` for the identical need
(read-only cross-repo access to `Blue`). No new App, no new secret surface.

**Blocker**: `BLUE_READ_APP_ID`/`BLUE_READ_APP_PRIVATE_KEY` are provisioned
as repo secrets on ZabCanvas; they are **not yet set on `Orion`**. CI on this
PR will fail at the "mint Blue-read App token" step until they're added here
(same App, just needs the Orion repo included in its installation/secret
scope — no rotation, no new key). This falls under `docs/rules/security.md`
secrets handling; provisioning needs a named clearance (Bastion) before
being applied, per bail `B3-R6-10-ORION-HOST-SPIKE`.

Refs #331
