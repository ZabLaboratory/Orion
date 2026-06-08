# Runbook — Solar deploy fix for dual-build tarball (`host/` subtree)

- **Date**: 2026-06-08
- **Author**: Keeper (operator agent)
- **Type**: urgent infra hotfix (auto-merged without tierce review — see "Hotfix justification")
- **Scope**: `Orion/.github/workflows/solar-deploy.yml` (CI/deploy infra only)
- **Related**: Solar ADR 001 (dual-build host bundle), Solar PR #10 (v0.2.1), Orion PR #59
- **Incident run**: `solar-deploy.yml` run 27129874469 (FAILURE)
- **Fix run**: `solar-deploy.yml` run 27130224418 (SUCCESS)

## Symptom

`solar-deploy.yml` (`workflow_dispatch solar_version=v0.2.1`) failed at the
"Install Solar bundle on VPS (idempotent)" step:

```
##[error]Solar v0.2.1 tarball has no index.html at root — refusing to install
```

Preflight (asset reachable, secrets present) and SSH setup were green; the
failure was the remote install script's root-`index.html` guard.

## Root cause (proven, not deduced)

Solar's dual-build (ADR 001), shipped in v0.2.1, restructured the release
tarball `solar-v0.2.1.tgz`. Verified by inspecting the published asset:

- **tarball root** now holds the **library entry** `./solar.js` (externals
  preserved, vendored by Prism) — this is **not servable**: it carries bare
  ESM specifiers a bare browser/CEF cannot resolve.
- **`./host/`** holds the **self-contained served bundle**: `host/index.html`
  + `host/assets/*` (deps inlined, **zero bare specifiers**) — the artefact the
  CEF/Orion static handler must serve.

`solar-deploy.yml` was written for the v0.2.0 **flat** layout: it asserted
`./index.html` at the tarball root and `cp -a "$stage/."` the whole tarball.
Post-dual-build that assertion fails (no root `index.html`), and had it passed
it would have served the wrong (library) root.

This is a **served-artefact internal-layout drift** introduced by B4. The
served *URL* contract (`/static/solar/v{N}/index.html` + relative JS) is
unchanged (ADR 001 §3.3) — only the tarball's internal shape moved.

## Fix

In the install script, resolve the **served root** before the atomic swap:

- prefer `$stage/host/index.html` (dual-build, v0.2.1+) → `served=$stage/host`
- else fall back to `$stage/index.html` (flat, ≤ v0.2.0) → `served=$stage`
- else fail with a clear error

Copy only `$served/.` into the version dir. The library `solar.js` never lands
in the served directory. Backward-compatible with v0.2.0.

Commit: `fix(deploy): serve host/ subtree for dual-build Solar tarball`
(Orion `main` @ 31e4c72, via PR #59).

## Verification

- Re-dispatched `solar-deploy.yml` v0.2.1 with `force_reinstall=true` (clears the
  partial dir from the failed run). Run 27130224418 — install + gateway
  health-check both SUCCESS.
- `GET https://zabgate.cyell.dev/orion/static/solar/v0.2.1/index.html` → **200**.
  `index.html` references `./assets/host-D7jDi5Eh.js` (host entry, not `solar.js`).
- Served host JS (349 KB, deps inlined) scanned: **zero bare specifiers**
  (`from "react"` / `@preact` / `framer-motion` / `motion` — none).
- VPS `~/orion/solar/v0.2.1/`: `index.html` + `assets/` + `.installed` marker;
  **no leaked library `solar.js`** in the served root.

## Rollback

The deploy is content-immutable per version (URL carries the version). To roll
back the *served bundle*, redeploy a prior version tag via `solar-deploy.yml`
(`solar_version=v0.2.0`) — it lands under `solar/v0.2.0/` and Pulsar's CEF URL
is repointed by the M8 wiring (`--solar-version`). To roll back the *workflow
change* itself: `git revert 31e4c72` on Orion `main` (restores the flat-only
install guard); the v0.2.0 deploy still works under it.

## Hotfix justification (git.md Gate point 2)

Auto-merged (PR #59) under the bounded hotfix exception — the three conditions:

1. **Urgent** — blocked the M8 final-line broadcast (the served bundle could not
   be installed; the prod deploy path was red).
2. **Infra-pure** — a single CI/deploy workflow YAML; no `src/**`, no app code,
   no auth/secrets/network surface. Served URL contract unchanged (ADR 001 §3.3),
   so no Conduit contract realignment and no Bastion-sensitive surface.
3. **Documented** — this runbook.

No Bastion veto existed on this surface; the exception did not override any veto.
Flagged to Eleven for Vigil a-posteriori control.
