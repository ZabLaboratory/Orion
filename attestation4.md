MERGE_ATTESTATION

Role: keeper
Agent-Thread: govulncheck-toolchain
Work-Unit: ORION-CI-TOOLCHAIN-PIN
Repo: ZabLaboratory/Orion
PR: #355 (keeper/orion-ci-toolchain-pin-1266 -> main)
SHA: bf17b3159991b4988905a8c7c6a508b35e7eb09e
Method: squash merge, --admin (hotfix exception, same class as #350/#352/#354).
Decision: urgent (last PR#349 blocker), infra-pure (ci.yml only). Sensitive-surface exception: Bastion CLEARED_WITH_CONDITIONS already acquired on Orion#336 for this exact change (DoS-only CVEs, no RCE/confidentiality/integrity impact, no veto) — clearance obtained prior to merge as required, not bypassed.
Residual risk: two closure conditions remain open, tracked separately (prod toolchain measurement, Dockerfile digest pin) — non-blocking per team-lead, do not affect this CI-only surface.
