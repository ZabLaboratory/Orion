# Orion workload identity (software-P256)

This runbook records the étage 1 wiring for the Orion workload route. It is
intentionally separate from a closure report: it documents the deployment
contract and the remaining qualification work.

## Identity and topology

The current dev VPS identity is:

```text
WORKLOAD_ENVIRONMENT=production
WORKLOAD_NAME=orion
WORKLOAD_INSTANCE_ID=orion-1
ORION_WORKLOAD_SAN=spiffe://zab/workload/orion/production/orion-1
```

Orion reaches the Gate workload mTLS sidecar at:

```text
https://zabgate-workload.internal:4402
```

The sidecar terminates client mTLS, validates the Orion certificate and
forwards the request to the Gate workload endpoint. The certificate authority
and Orion client material are mounted from the VPS only:

```text
/etc/zab/workload-orion/orion-client-cert.pem
/etc/zab/workload-orion/orion-client-key.pem
/etc/zab/workload-orion/ca.pem
/etc/zab/workload-orion/canvas-trust.json
```

`docker-compose.prod.yml` mounts `/var/lib/zab/workload-orion` read-only at
that path. The Orion image runs as the distroless nonroot user (UID 65532), so
the directory must be traversable by that user, the private key must be mode
`0600` and owned by UID 65532, and public certificate/trust files may be
read-only.

## Configuration contract

The deploy workflow writes the following non-secret values from the Orion
`production` environment variables:

```text
ORION_WORKLOAD_ZABGATE_URL
ORION_WORKLOAD_CLIENT_CERT_PATH
ORION_WORKLOAD_CLIENT_KEY_PATH
ORION_WORKLOAD_CA_PATH
ORION_WORKLOAD_SAN
ORION_CANVAS_TRUST_PATH
ORION_CANVAS_LOCATOR_PREFIX
ORION_OWNER_ID
ORION_TENANT_ID
```

The private key, issued certificate, CA and Canvas trust material must never be
committed, copied into the image, printed in CI, or placed in GitHub variables.
Only the file paths and identity metadata belong in the deploy configuration.

`ORION_OWNER_ID` and `ORION_TENANT_ID` are not arbitrary defaults. They must
match the owner and tenant claims in the Canvas resolved-scene-ref that Orion
is expected to consume. The values used for the current dev activation were
read from a live Canvas issuance: owner
`7b33a262-781f-464f-b3c7-e6f1f3c0fe9f`, tenant
`adr-blue-012-fixture`. The tenant name identifies a fixture and is not proof
of a production business tenant.

## Provisioning and rotation

The Orion client certificate was issued by ZabAuth through a workload challenge
bound to the real node proof key and the exact Orion SAN. The current Auth
policy caps workload certificates at 600 seconds. This means the first
provisioned certificate is a bootstrap artifact, not a complete production
rotation solution.

Before expiry, a dedicated Orion identity-agent must obtain the next
certificate through the same challenge/certificate flow, write the key and
certificate to a new directory, validate the key pair and SAN locally, then
atomically switch the VPS path and recreate Orion. Keep the old material until
the new container is healthy, then remove only the expired certificate/key
pair. Never log certificate bodies, private keys, challenge responses or
secret values.

## Qualification checklist

The route is eligible for further validation only after all of these are
observable on the target VPS:

1. Orion starts with the read-only identity mount and logs that the scene-intent
   surface is wired.
2. The client certificate chains to the current workload CA, has clientAuth
   EKU, and has the exact Orion SAN.
3. A real Canvas resolved-scene-ref is accepted through Gate and a mismatched
   owner, tenant, locator, signature, expired reference or wrong SAN is
   rejected.
4. Plaintext access to the mTLS sidecar and spoofed identity headers are
   rejected.
5. Certificate rotation completes before the 600-second TTL and Orion remains
   healthy across the replacement.

Until those checks are recorded, this wiring is a dev-VPS deployment step and
not a production qualification or closure of the related issues.
