# Runbook — Deploy Solar v0.2.13 (positional invitee-camera slots) to the VPS

**Date:** 2026-07-01 · **Author:** Keeper · **Host:** `vps-ovh` (51.91.126.43, `ubuntu`)

## Context / root need

Positional invitee-camera slots (`@0` / `@1` / `@2` → first-come by arrival
order) live in `Solar/src/peer-viewer/slot-binding.ts` (PR #33), shipped in
Solar **v0.2.13**. That build is vendored inside Prism
(`resources/solar/v0.2.13/`) so the **local preview** already renders the slots,
but the **antenne** (Pulsar CEF loading Orion static) could only reach up to
**v0.2.11** on the box — so on-air the positional slots did not render.

Fix: publish the vendored v0.2.13 dist into Orion's static Solar dir on the VPS.

## Why no redeploy is needed

`internal/api/static.go::staticSolarHandler` is a bare `http.FileServer` over
the configured root. The prod compose bind-mounts the host dir read-only:

```
docker-compose.prod.yml:  - ./solar:/var/lib/orion/solar:ro
```

There is **no version allowlist** in code — `SOLAR_VERSIONS` in `deploy.yml`
only drives the DR install/fetch loop, not serving. A new `v0.2.13/host/`
subdir under `./solar` is served the instant it lands; Orion is not restarted.

## Deploy steps (executed)

Source dist: `D:\Documents\Zab\Prism\resources\solar\v0.2.13\` (the exact
artifact the Prism preview validated). Layout = `index.html` + `assets/` at
root; on the box each version wraps that payload in a `host/` subdir
(`solar/v{N}/host/index.html` + `host/assets/`), matching v0.2.11.

Verified the dist carries the positional code before pushing:
`generator = @zablab/solar 0.2.13`; `slot-binding.ts` + `positional` /
`positionalKeys` / `positionalValues` present in `assets/host-*.js(.map)`.

```bash
# 1. Tar the host payload (index.html + assets/) from the vendored dist
cd resources/solar/v0.2.13 && tar --force-local -czf solar-v0.2.13-host.tgz index.html assets

# 2. Push + extract into the StaticDir (additive; no existing version touched)
scp solar-v0.2.13-host.tgz vps-ovh:/tmp/
ssh vps-ovh '
  T=/home/ubuntu/orion/solar/v0.2.13/host
  mkdir -p "$T"
  tar -xzf /tmp/solar-v0.2.13-host.tgz -C "$T"
  touch /home/ubuntu/orion/solar/v0.2.13/.installed   # skip DR fetch loop (no Release exists)
  rm -f /tmp/solar-v0.2.13-host.tgz
'
```

## Verification (proof)

```
GET https://zabgate.cyell.dev/orion/static/solar/v0.2.13/host/index.html
  → 301 → ./ → 200   (canonical FileServer redirect, identical to every version)
  body: <meta ... content="@zablab/solar 0.2.13">
GET .../v0.2.13/host/assets/host-BJDUyLgJ.js  → 200   (bundle carrying slot-binding)
GET .../v0.2.11/host/index.html               → 200   (existing version untouched)
```

## Go-live (porteur / Eleven — NOT done here)

The antenne browser-source is **not** repointed by this runbook. At go-live,
point the Pulsar CEF browser-source to:

```
https://zabgate.cyell.dev/orion/static/solar/v0.2.13/host/index.html
  ?orion=<wss .../orion/api/v1/show/stream.lsdp?token=SHOW>
  &mode=broadcast
```

(scrub the `?token=eyJ…` show-token JWT from CEF logs after the test — see
`_shared/live-testing.md`.)

## Rollback (instant, non-destructive)

Repoint the antenne browser-source back to
`.../orion/static/solar/v0.2.11/host/index.html` (still on the box, unchanged).
No server action, no restart. The v0.2.13 dir can also be removed if ever
needed — it is purely additive and referenced by no other version.

## Residual debt

- v0.2.13 (like v0.2.10 / v0.2.11) has **no GitHub Release** → a fresh-box DR
  would fail its fetch. Listed in `deploy.yml SOLAR_VERSIONS` with an
  `.installed` marker so the live box skips the fetch. Reproducibility needs a
  real Solar Release (restore `release.yml`) or a re-hot-push from vendored dist.
- v0.2.12 is vendored in Prism but was never pushed to the box; intentionally
  omitted from `SOLAR_VERSIONS`.
