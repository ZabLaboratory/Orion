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
| `ORION_PUBLIC_BASE_URL` | `https://zabgate.cyell.dev/orion` | Public base used when Orion returns URLs |
| `ORION_PUBLIC_WHIP_BASE` | `https://orion-media.cyell.dev` | The browser's WHIP target |

### Shared networks on the VPS

The compose file references two external Docker networks:

```bash
docker network create zab-internal     # if not already present
docker network create caddy-public     # if not already present
```

`zab-internal` is where all Zablab services talk to each other. `caddy-public`
is what the Caddy container joins to reach exposed services.

### Caddy

See [`Caddyfile.snippet`](./Caddyfile.snippet) — drop it into `/etc/caddy/Caddyfile` and reload.

You need DNS:

- `orion-media.cyell.dev` → VPS IP (new A/AAAA record for MediaMTX)

`zabgate.cyell.dev` already fronts the gateway, so `/orion/*` routes
work out of the box once ZabGate is redeployed with the new `/orion`
upstream.

## Local smoke test (already verified during scaffolding)

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

If `/health` returns 200 and the MediaMTX container is up, the stack is ready.
First streaming attempt will create a path on the fly via the control API.
