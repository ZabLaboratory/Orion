# Feature module refactor — 2026-09-19

Owner: Eleven (current task). Work unit: local-20260919-feature-modules.

User mandate: organize scene-related implementation by feature, preserve global logic and contracts, merge every refactored service, then revalidate the merged stack. Blue is excluded. No Pulsar, dependency, database-schema, design or transport change.

Repository: ZabLaboratory/Orion. Branch: codex/local-20260919-feature-modules.

## Acceptance and evidence

| Criterion | Risk | Validation | Evidence |
| --- | --- | --- | --- |
| Feature extraction preserves implementation | Import cycles, lost state, changed ordering | Mechanical body comparison, lint/typecheck, existing full suite | Local command results and PR CI |
| Existing entry points still work | Consumer imports or HTTP contract drift | Re-exports and existing contract tests | Full suites and builds |
| Merged services compose correctly | Stale local artifacts | Rebuild local Orion/Solar/Prism; Preview E2E after merge | Editable, switch, component and toolbox/audio proofs |
| Blue and Pulsar unchanged | Build helpers mutate sibling sources | Before/after Git status and diff fingerprints | Final audit |

Pre-existing caveat: the preceding editable Preview E2E failed once on native camera pixel change, then passed unchanged. A rerun alone is not proof that this intermittent issue is fixed.

Merge gate: local tests and required PR CI green, signed commit, pre-merge attestation; verify merge SHA on origin/main. Retest all modified services after merge. Rollback: revert each refactor commit independently. No feature or architectural redesign is included.
