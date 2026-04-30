# Orion — Deployment notes

## What the `Deploy` GitHub Actions workflow needs

### Secrets (set on the `ZabLaboratory/Orion` repo under Settings → Secrets → Actions)

| Secret | Value | Notes |
|---|---|---|
| `VPS_HOST` | IPv4 / DNS of the VPS | — |
| `VPS_USERNAME` | SSH user on the VPS | deploy user with docker group |
| `VPS_SSH_KEY` | Private SSH key in PEM form | Matches a public key in `~/.ssh/authorized_keys` on the VPS |
| `VPS_APP_PATH` | Absolute path, e.g. `/opt/orion` | Must exist, must be writable by `VPS_USERNAME` |
| `ORION_PG_PASSWORD` | Strong random password | `python -c "import secrets; print(secrets.token_urlsafe(24))"` |
| `ORION_ENCRYPTION_KEY` | AES-GCM 32-byte urlsafe-b64 key | `python -c "import secrets,base64; print(base64.urlsafe_b64encode(secrets.token_bytes(32)).decode())"` |
| `TWITCH_CLIENT_ID` | From https://dev.twitch.tv/console/apps | Create an app with type "Server-to-Server" or "Web" |
| `TWITCH_CLIENT_SECRET` | From https://dev.twitch.tv/console/apps | — |
| `ORION_TWITCH_OAUTH_REDIRECT_URI` | `https://zablab.cyell.dev/orion/credentials` | Must match the Twitch app's registered redirect URL |

> **Removed at the v0.4.0 pivot** : `ORION_PUBLIC_BASE_URL`, `ORION_PUBLIC_WHIP_BASE`. Streaming now runs through Pulsar in Prism — Orion no longer fronts a media plane and the public WHIP subdomain is no longer needed.

### Shared networks on the VPS

The compose file references one external Docker network:

```bash
docker network create zab-internal     # if not already present
```

`zab-internal` is where all Zablab services talk to each other. Orion's API surface is reached through ZabGate (already on `zab-internal`) ; there is no public subdomain dedicated to Orion.

### Caddy

No dedicated Caddy entry is required for Orion. Routes pass through `zabgate.cyell.dev/orion/*` per the gateway routing in `ZabGate/src/zabgate/config.py`.

> **Removed at the v0.4.0 pivot** : the `orion-media.cyell.dev` subdomain that fronted MediaMTX. DNS records for that subdomain can be retired.

## Local smoke test

```bash
# From the Orion/ repo root:
docker network create zab_network    # one-off
docker compose up -d --build
docker compose exec orion alembic upgrade head
curl http://localhost:4007/health
```

Expected: `{"status":"ok","service":"orion","database":"connected"}`.

## Post-deploy verification

```bash
# From a shell on the VPS:
docker run --rm --network zab-internal curlimages/curl:latest \
  -sf http://orion:4007/health
# → {"status":"ok","service":"orion","database":"connected"}

# Through the gateway (also requires ZabGate redeploy with /orion upstream):
curl -sf https://zabgate.cyell.dev/orion/health
```

If `/health` returns 200 the stack is ready.
