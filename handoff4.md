AGENT_REPORT

Role: keeper
Agent-Thread: govulncheck-toolchain
Work-Unit: ORION-CI-TOOLCHAIN-PIN
Repo: ZabLaboratory/Orion
Issue: 336 / PR #349 (target) / PR #355 (fix)

Result: pinned go-version from wildcard "1.26.x" to exact "1.26.6" across all 8 Go CI jobs (commit bf17b31, PR #355). Closes the 6 govulncheck stdlib CVEs (GO-2026-6218/6090/6089/6088/5972/5026), all fixed upstream in go1.26.6, previously unreachable because the wildcard kept resolving to 1.26.5 on this runner.

Bastion clearance already acquired (CLEARED_WITH_CONDITIONS, Orion#336) before this change: all 6 CVEs DoS-only, no RCE/confidentiality/integrity impact, no veto on the pin itself. Two closure conditions remain, explicitly out of this work unit's scope per team-lead: (a) measure the actual toolchain compiling the prod binary (Dockerfile:4 `FROM golang:1.26-alpine` floating, deploy.yml:233 `docker compose build` without --pull), (b) pin by digest (closes drift also flagged for postgres:16-alpine in conventions.md §Docker).

Hotfix exception requested: urgent (last remaining PR#349 blocker), infra-pure (ci.yml only), documented after (runbook update to follow), Bastion-cleared for the surface it touches.

Status: READY
