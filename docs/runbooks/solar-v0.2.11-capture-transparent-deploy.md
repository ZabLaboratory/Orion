# Runbook — Solar v0.2.11 manual deploy (transparent host = on-air capture fix)

- **Date**: 2026-06-24
- **Author**: Keeper (operator agent)
- **Type**: prod VPS deploy + infra workflow edit (auto-merged — see "Hotfix justification")
- **Scope**: VPS `vps-ovh` `/home/ubuntu/orion/solar/v0.2.11/host/` (new, additive) +
  `Orion/.github/workflows/deploy.yml` (`SOLAR_VERSIONS` pin). No app code.
- **Related**: Solar capture-source on-air path; `agents/_shared/live-testing.md`;
  runbooks `solar-deploy-and-lsdp-flip.md`, `solar-deploy-host-subtree-hotfix.md`.

## Goal

Serve Solar **v0.2.11** from the box so capture scenes (Pulsar native cam/screen
sources) render on-air. v0.2.11's `host/index.html` switched the body background
from `#000` (opaque — voiled native sources behind the CEF browser_source with
black) to **`background: transparent`**, so native capture sources anchored
behind the transparent `x-zab.capture` placeholder show through on-air. Without
v0.2.11 served by the VPS, capture scenes stay black on-air.

## Why manual (not deploy.yml / solar-deploy.yml)

Solar no longer has `release.yml` nor a GitHub Release for v0.2.10 / v0.2.11.
Both standard paths (`deploy.yml` `SOLAR_VERSIONS` install loop, and the
on-demand `solar-deploy.yml`) pull `solar-<tag>.tgz` anonymously from the Solar
**GitHub Release** — which does not exist for these tags. So the bundle was
**hot-pushed manually** from the local build `D:\Documents\Zab\Solar\dist\host`
(generator `@zablab/solar 0.2.11`, identical to the vendored Prism
`resources/solar/v0.2.11`). Same mechanism used for v0.2.10.

## Procedure (executed)

1. SSH preflight: `ssh vps-ovh` OK; confirmed v0.2.0–v0.2.10 present, v0.2.11 absent.
2. Confirmed v0.2.10 served layout is the **host subtree** (`v0.2.10/host/` with
   `index.html` + `assets/`, `.installed` marker at the version root) — mirrored exactly.
3. Verified local `dist/host`: `background: transparent` in `index.html`,
   `x-zab.capture` present in `assets/host-DFzP_bOA.js`, generator `0.2.11`.
4. Pushed: `tar -czf - index.html assets` (from `dist/host`) piped over SSH,
   extracted into a `mktemp` staging dir, asserted `index.html` + `assets/`
   present, copied into `/home/ubuntu/orion/solar/v0.2.11/host/`, then
   `touch .installed` **last** (success marker).
5. Permission fix: the `mktemp` dir was `700`; reset `host/` tree to `755` dirs /
   `644` files to match v0.2.10 (the orion container reads the bind-mount
   read-only and needs traversal). No container restart — the static handler
   reads the live host dir (per `solar-deploy-and-lsdp-flip.md` §1).
6. `deploy.yml` `SOLAR_VERSIONS` bumped `"v0.2.8 v0.2.9"` →
   `"v0.2.8 v0.2.9 v0.2.10 v0.2.11"` (v0.2.10 was also missing — durability).

## Verification (proof)

Via gateway `https://zabgate.cyell.dev/orion/static/solar`:

- `GET /v0.2.11/host/` (-L) → **200**, `Content-Type: text/html`.
  (`/v0.2.11/host/index.html` direct → `301 → ./`, expected Go static redirect.)
- Served `index.html`: `background: transparent` present; `background:#000`
  count **0**; generator `@zablab/solar 0.2.11`.
- Served `assets/host-DFzP_bOA.js`: `x-zab.capture` present (1 match).
- No regression: `v0.2.8 / v0.2.9 / v0.2.10 host/` → **200** each.
- Orion `health=200`, `ready=200`.

## Rollback

Versions are path-immutable and additive — v0.2.11 did not touch v0.2.8/9/10.
To roll a consumer back: re-point its browser-source / `DEFAULT_SOLAR_VERSION`
to `v0.2.10` (still on the box, instant — its host is the same JS, only the
opaque `#000` background differs). To remove the bad bundle entirely:

```bash
ssh vps-ovh "rm -rf /home/ubuntu/orion/solar/v0.2.11"
```

Affects only clients requesting that exact version path; no restart.
Workflow rollback: `git revert` the `SOLAR_VERSIONS` commit on Orion `main`
(restores `"v0.2.8 v0.2.9"`); the live box is unaffected (install loop is
idempotent on `.installed`).

## Durability debt (open)

v0.2.10 and v0.2.11 are **not reproducible** via `deploy.yml` / `solar-deploy.yml`
because no GitHub Release exists for them (Solar lost `release.yml`). On the live
box this is inert: the install loop skips any version already marked `.installed`,
so it never attempts the (404) Release fetch. **But a fresh box (disaster
recovery) would fail** on the curl for v0.2.10/v0.2.11. To close: cut real Solar
Releases for these tags (restore `release.yml`), or vendor a dist and re-hot-push.
Tracked in the `deploy.yml` `SOLAR_VERSIONS` comment.

## Hotfix justification (git.md Gate point 2)

Auto-merged under the bounded hotfix exception — the three conditions:

1. **Urgent** — capture scenes are black on-air without v0.2.11 served; the
   on-air capture path is broken until the box serves the transparent host.
2. **Infra-pure** — a static bundle push to the VPS + one workflow YAML
   (`SOLAR_VERSIONS` pin). No `src/**`, no app code, no auth/secrets/network
   surface. Served URL contract unchanged (additive version path). No new host
   port, no Conduit contract realignment.
3. **Documented** — this runbook.

No Bastion veto existed on this surface; the exception did not override any veto.
Flagged to Eleven for Vigil a-posteriori control.
