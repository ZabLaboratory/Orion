AGENT_REPORT

Role: keeper
Agent-Thread: gomod-cache-fix
Work-Unit: ORION-CI-GOMOD-CACHE-FIX2
Repo: ZabLaboratory/Orion
Issue: 336 / PR #349 (target) / PR #352 (fix v2)

Result: PR #350's conditional purge did not work. Root cause of the miss: go's internal git codehost bare repos under GOMODCACHE/cache/vcs/<hash> do not carry a remote named `origin` (confirmed by Forge's log evidence on PR#349 rebase run 31760522179 — purge loop executed, same poisoned hash dir untouched, same "Invalid username or token"). Fix v2 (PR #352, commit cfcdf09): unconditional `rm -rf "$(go env GOMODCACHE)/cache/vcs"` before every `go mod download`, all 8 Go jobs. Safe/cheap: only Blue is fetched via direct VCS (GOPRIVATE/insteadOf); every other module resolves through GOPROXY's separate cache/download path, unaffected.

Hotfix exception requested: urgent (PR#349 still blocked after first attempt), infra-pure (ci.yml only), documented after (runbook update to follow).

Status: READY
