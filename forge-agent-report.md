AGENT_REPORT

Role: forge
Agent-Thread: bump-lumencast-pin
Work-Unit: ORION-BUMP-LUMENCAST-SNAPSHOT-METADATA
Repo: ZabLaboratory/Orion
Issue: 334 (journal only — not closed)
Branch: forge/bump-lumencast-snapshot-metadata
Commit: 914e450938f28f231568d946370b30f8bac89e24
PR: https://github.com/ZabLaboratory/Orion/pull/384
Status: READY

Result: pin bumped to lumencast-go v0.3.2-0.20260816093300-0c7cfc65c694
(0c7cfc6, #22). Confirmed Orion's Snapshot forward path (mirror.go's
m.scene.Set -> kit refreshAll -> stampMetadata) now automatically carries a
scene's known projection identity, because sceneMirror.recordIdentity and
the kit Scene's lastMetadata update on the same EmitWithCauseAndMetadata
call against the same *lserver.Scene. Fixed
orion_lsdp_snapshot_identity_gap_total accordingly: no longer incremented
when identity is known (would be a permanent false positive), kept wired as
a regression guard, "nothing known yet" branch untouched.

Criteria -> evidence:
- go build ./... : clean.
- go vet ./... : clean.
- go test ./internal/lsdp/... ./internal/obs/... -v : all pass, incl. 2
  rewritten SnapshotIdentityGap tests + 1 new e2e wire-level test
  (TestLSDP_SnapshotForwardCarriesKnownProjectionIdentity).
- go test ./... : all packages ok.
- go.sum coherent (git diff --stat: go.mod +1/-1, go.sum +2/-2).
- PR opened, not merged by me.

Risks / gaps: SnapshotMetrics seam now has zero live call sites that
increment it under current code (proven structurally, not just observed) —
kept per instruction, not removed. internal/runtime (Engine A) untouched.
No incompatibility found from the bump; go.mod/go.sum only diff is the pin.
