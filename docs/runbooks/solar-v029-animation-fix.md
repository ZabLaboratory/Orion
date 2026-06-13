# Runbook — Solar v0.2.9 animation fix (core.animation.play@1)

> Owner: Keeper. Surface: VPS `vps-ovh` (`ubuntu@51.91.126.43`), app path
> `/home/ubuntu/orion`, container `orion` on `zab-internal`.
> Refs: Solar PR #24 · Orion PR #168 (`8b1174e`) · ADR 011.

---

## 1. Diagnostic — why `core.animation.play@1` rendered statically on Solar v0.2.8

Two independent bugs in `@lumencast/runtime@0.6.0` (the version vendored in
Solar v0.2.8) prevented any animation from playing at the antenna:

### 1.1 Box geometry: `display:contents` breaks transform anchoring

`KeyframePlayer` wrapped the animated element in a box node. That box was
rendered with `display:contents`, which removes it from the layout tree —
transforms applied to it have no visual effect because there is no rendered
box to anchor them against.

**Symptom**: a frame-diff across an `animation.play` firing shows zero luma
delta — the element remains pixel-identical before and after the generation
leaf triggers.

### 1.2 Framer Motion prop name: `translateX` instead of `x`

The keyframe geometry emitted by the Orion compiler used `translateX`/`translateY`
as the framer-motion motion prop keys. Framer Motion 12 expects the shorthand
`x`/`y` for translate axes. `translateX` is an unrecognised prop and silently
no-ops — the element does not translate.

**Symptom**: same zero-delta frame-diff even if the box anchoring were correct.

---

## 2. Fix — Solar #24 / tag v0.2.9

Both bugs are patched via `patch-package` on `@lumencast/runtime@0.6.0`.
The patch is included in the Solar v0.2.9 release; **no changes to Orion
compiler or runtime were required for this fix**.

| Bug | Patch |
|---|---|
| Box `display:contents` | `position:absolute; inset:0` (anchors the transform box in the layout flow) |
| `translateX`/`translateY` props | renamed to `x`/`y` (framer-motion 12 shorthand) |

The `__anim.<overlay_id>` scalar leaf shape (ADR 011 §3.2 — object→scalar
wire fix) is a **separate compiler/runtime change** landed in Orion independently.
Solar v0.2.9 addresses only the rendering side.

---

## 3. Integration — Orion side

### 3.1 ci.yml SOLAR_VERSIONS reconciliation (PR #168 / `8b1174e`)

`SOLAR_VERSIONS` in `.github/workflows/ci.yml` was updated from the stale
`v0.1.1` pin to `"v0.2.8 v0.2.9"`:

```yaml
# .github/workflows/ci.yml (deploy job, env section)
SOLAR_VERSIONS: "v0.2.8 v0.2.9"
```

Both versions are installed on the VPS at each deploy (idempotent — a version
already present is a no-op). v0.2.8 is kept so existing browser-source URLs
do not break.

### 3.2 VPS install — Solar v0.2.9

Path: `/home/ubuntu/orion/solar/v0.2.9/`

The bundle is installed by the deploy job (step "Install Solar bundles") on
every push to `main`. Manual install (if the deploy job did not run):

```bash
V=v0.2.9
ssh vps-ovh "
  set -euo pipefail
  target=/home/ubuntu/orion/solar/\$V
  if [ -f \"\$target/.installed\" ]; then echo 'already installed'; exit 0; fi
  mkdir -p \"\$target\"
  tmp=\$(mktemp -d)
  curl -fsSL --retry 3 -o \"\$tmp/solar-\$V.tgz\" \
    https://github.com/ZabLaboratory/Solar/releases/download/\$V/solar-\$V.tgz
  tar -xzf \"\$tmp/solar-\$V.tgz\" -C \"\$target\"
  test -f \"\$target/index.html\"
  touch \"\$target/.installed\"
  rm -rf \"\$tmp\"
"
# verify via gateway:
curl -fsSL -o /dev/null -w '%{http_code}\n' \
  https://zabgate.cyell.dev/orion/static/solar/v0.2.9/index.html
# expect: 200
```

**No container restart required** — the solar/ directory is a read-only bind
mount; the static handler reads the live host directory.

### 3.3 go-live URL bump

Pulsar browser-source and any hardcoded Solar URL must be updated to v0.2.9:

```
.../orion/static/solar/v0.2.9/host/index.html
  ?orion=<wss .../show/stream.lsdp?token=SHOW>
  &mode=broadcast
```

v0.2.8 URLs continue to work (the bundle is still installed); the bump is
required to get the animation fix.

---

## 4. Proof — I7 re-tir (2026-06-13)

Test harness: `core.animation.play@1` targeting an overlay, firing the
generation leaf.

| Metric | Value |
|---|---|
| Observed | Box glides from centerX 131 → 450 + fade |
| Frame-diff measure | Spatial stddev: luma delta detected across the animation window |
| RTMP output | 14.68 MB encoded |
| Recording | `pulsar-20260613-145030.mp4` |
| Solar version | v0.2.9 (CEF browser source) |
| Orion commit | `8b1174e` (main after PR #168) |

Criteria satisfied (ADR 011 §6 R1 — « Observable movement on air »): a real
`.mp4` capture shows frame-diff-detectable movement. The proof was performed
against a live RTMP stream, not a headless screenshot.

---

## 5. Rollback

Solar versions are path-immutable and additive. v0.2.8 is always installed
alongside v0.2.9. To roll back animation rendering to v0.2.8:

1. Point the Pulsar browser-source URL at v0.2.8:
   ```
   .../orion/static/solar/v0.2.8/host/index.html?orion=...&mode=broadcast
   ```
2. No Orion change, no container restart needed.

If v0.2.9 is found to cause a regression beyond animation:

```bash
# Remove the v0.2.9 bundle from VPS (does not affect v0.2.8)
ssh vps-ovh "rm -rf /home/ubuntu/orion/solar/v0.2.9"
```

To keep v0.2.9 off future deploys, revert the `SOLAR_VERSIONS` line in
`ci.yml` to `"v0.2.8"` via a PR (do not modify the line directly on the VPS —
the next deploy would reinstall it).

---

## 6. Quick reference

| Check | Command | Healthy |
|---|---|---|
| v0.2.9 bundle served | `GET /orion/static/solar/v0.2.9/index.html` (-L) | 200 |
| v0.2.8 bundle still served | `GET /orion/static/solar/v0.2.8/index.html` (-L) | 200 |
| VPS install marker | `ls /home/ubuntu/orion/solar/v0.2.9/.installed` (via ssh) | file exists |
| Animation movement | Re-tir I7: fire `core.animation.play@1`, record `.mp4`, ffmpeg luma delta | non-zero stddev |
