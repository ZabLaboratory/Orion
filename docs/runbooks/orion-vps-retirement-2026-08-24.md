# Orion VPS retirement

Status: retired from the Prism execution path on 2026-08-24.

## Runtime decision

Prism executes Orion locally on loopback. Orion embeds the Go Blue runtime
compiled from the pinned Blue/runtime/go checkout. Prism verifies the
embedded runtime descriptor and source digest before opening the studio.
The remote Blue service remains an authoring and compute service; it is not the
Blue execution runtime.

## Remote action performed

The host vps-ovh (ubuntu@51.91.126.43) was inspected first. The Orion compose
project was /home/ubuntu/orion/docker-compose.prod.yml. The following services
were stopped with docker compose stop:

- orion
- orion-db
- orion-workload-identity-agent

Docker volumes and the compose project were intentionally retained for
rollback. No database or credentials were deleted.

## Rollback

Rollback is an explicit operator action only:

    ssh vps-ovh
    docker compose -f /home/ubuntu/orion/docker-compose.prod.yml start \
      orion-db orion-workload-identity-agent orion

The GitHub deploy workflow is manual-only and requires the
deploy_retired_remote=true input before it can redeploy the retired remote
service.

## Verification criteria

- Prism logs Blue embedded runtime verified before the studio gate completes.
- Prism routes every /orion/* request to 127.0.0.1; it does not fall back to
  ZabGate or the VPS.
- Preview and on-air scene-intent requests return from the local Orion sidecar.
- The local wire-only production proof exits with status 0.
