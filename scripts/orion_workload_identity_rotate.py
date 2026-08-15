#!/usr/bin/env python3
"""Rotate Orion's short-lived workload certificate on the VPS.

The deployment substrate owns the private key and the replacement decision.  The
identity-agent owns the node proof and the mTLS hop to ZabAuth.  This script is
the small host-side coordinator between them: it creates a fresh P-256 key and
CSR, asks the agent for a challenge/certificate exchange, validates the returned
identity locally, replaces the files, restarts Orion, and restores the previous
pair if Orion does not become healthy.

Only status, identity metadata, and error codes are logged.  Certificate bodies,
private keys, challenges, and response payloads are never printed.
"""

from __future__ import annotations

import fcntl
import hashlib
import http.client
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any


WORKLOAD = "orion"
ENVIRONMENT = "production"
INSTANCE_ID = "orion-1"
NODE_ID = "node-1"
AGENT_ID = "identity-agent-1"
EXPECTED_SAN = f"spiffe://zab/workload/{WORKLOAD}/{ENVIRONMENT}/{INSTANCE_ID}"

MATERIAL_ROOT = Path(os.environ.get("ORION_WORKLOAD_ROOT", "/var/lib/zab/workload-orion"))
CERT_PATH = MATERIAL_ROOT / "orion-client-cert.pem"
KEY_PATH = MATERIAL_ROOT / "orion-client-key.pem"
CA_PATH = MATERIAL_ROOT / "ca.pem"
AGENT_SOCKET = Path(
    os.environ.get(
        "ORION_WORKLOAD_IDENTITY_AGENT_SOCKET",
        "/var/lib/zab/workload-orion-agent/workload-agent.sock",
    )
)
LOCK_PATH = Path("/run/lock/orion-workload-identity-rotation.lock")
ROTATE_BEFORE_SECONDS = 240
HEALTH_TIMEOUT_SECONDS = 60


class RotationError(RuntimeError):
    """A fail-closed rotation error safe to expose in the unit log."""


def log(event: str, **fields: object) -> None:
    rendered = " ".join(f"{key}={value}" for key, value in fields.items())
    print(f"orion-workload-identity {event}{(' ' + rendered) if rendered else ''}", flush=True)


def run_command(
    args: list[str],
    *,
    input_bytes: bytes | None = None,
    check: bool = True,
) -> subprocess.CompletedProcess[bytes]:
    result = subprocess.run(args, input=input_bytes, capture_output=True, check=False)
    if check and result.returncode != 0:
        raise RotationError(f"command_failed:{args[0]}")
    return result


def cert_is_valid_for(seconds: int, path: Path = CERT_PATH) -> bool:
    if not path.is_file():
        return False
    result = run_command(
        ["openssl", "x509", "-in", str(path), "-checkend", str(seconds), "-noout"],
        check=False,
    )
    return result.returncode == 0


class UnixHTTPConnection(http.client.HTTPConnection):
    """HTTPConnection that speaks to the identity-agent Unix socket."""

    def __init__(self, socket_path: Path, timeout: float = 15.0) -> None:
        super().__init__("localhost", timeout=timeout)
        self.socket_path = socket_path

    def connect(self) -> None:  # noqa: D401 - required HTTPConnection hook
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(str(self.socket_path))
        self.sock = sock


def agent_post(path: str, payload: dict[str, Any]) -> dict[str, Any]:
    if not AGENT_SOCKET.exists():
        raise RotationError("agent_socket_unavailable")
    connection = UnixHTTPConnection(AGENT_SOCKET)
    try:
        body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        connection.request(
            "POST",
            f"/{path.lstrip('/')}",
            body=body,
            headers={"Content-Type": "application/json"},
        )
        response = connection.getresponse()
        raw = response.read()
    except (OSError, http.client.HTTPException) as exc:
        raise RotationError("agent_transport_failed") from exc
    finally:
        connection.close()

    try:
        decoded = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise RotationError(f"agent_response_invalid:{response.status}") from exc
    if not isinstance(decoded, dict):
        raise RotationError(f"agent_response_invalid:{response.status}")
    if response.status != 201:
        detail = decoded.get("detail")
        code = detail.get("code") if isinstance(detail, dict) else None
        raise RotationError(f"agent_http_{response.status}:{code or 'unknown'}")
    return decoded


def public_key_digest_from_key(path: Path) -> bytes:
    result = run_command(["openssl", "pkey", "-in", str(path), "-pubout", "-outform", "der"])
    return hashlib.sha256(result.stdout).digest()


def public_key_digest_from_certificate(path: Path) -> bytes:
    certificate = run_command(["openssl", "x509", "-in", str(path), "-pubkey", "-noout"])
    public_key = run_command(["openssl", "pkey", "-pubin", "-outform", "der"], input_bytes=certificate.stdout)
    return hashlib.sha256(public_key.stdout).digest()


def validate_material(cert_path: Path, key_path: Path) -> None:
    if public_key_digest_from_key(key_path) != public_key_digest_from_certificate(cert_path):
        raise RotationError("certificate_key_mismatch")

    details = run_command(["openssl", "x509", "-in", str(cert_path), "-text", "-noout"]).stdout.decode(
        "utf-8", errors="replace"
    )
    if f"URI:{EXPECTED_SAN}" not in details:
        raise RotationError("certificate_san_mismatch")
    if "TLS Web Client Authentication" not in details:
        raise RotationError("certificate_client_eku_missing")
    if not cert_is_valid_for(30, cert_path):
        raise RotationError("certificate_expired_or_near_expiry")


def write_private_file(path: Path, content: str) -> None:
    path.write_text(content, encoding="ascii")
    os.chown(path, 65532, 65532)
    os.chmod(path, 0o600)


def write_public_file(path: Path, content: str) -> None:
    path.write_text(content, encoding="ascii")
    os.chown(path, 1000, 1000)
    os.chmod(path, 0o644)


def docker_restart_and_wait() -> None:
    run_command(["docker", "restart", "orion"])
    deadline = time.monotonic() + HEALTH_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        result = run_command(
            ["docker", "inspect", "--format", "{{.State.Health.Status}}", "orion"],
            check=False,
        )
        if result.returncode == 0 and result.stdout.strip() == b"healthy":
            return
        time.sleep(2)
    raise RotationError("orion_health_timeout")


def hardlink_previous(previous_dir: Path) -> bool:
    previous_dir.mkdir(mode=0o700)
    found = False
    for source in (CERT_PATH, KEY_PATH, CA_PATH):
        if source.exists():
            os.link(source, previous_dir / source.name)
            found = True
    return found


def restore_previous(previous_dir: Path) -> None:
    for name in (CERT_PATH.name, KEY_PATH.name, CA_PATH.name):
        source = previous_dir / name
        if source.exists():
            os.replace(source, MATERIAL_ROOT / name)


def rotate() -> None:
    if not AGENT_SOCKET.exists():
        raise RotationError("agent_socket_unavailable")
    MATERIAL_ROOT.mkdir(mode=0o755, parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=".orion-workload-rotation-", dir=str(MATERIAL_ROOT)))
    previous = staging / "previous"
    key = staging / KEY_PATH.name
    csr = staging / "orion-client.csr.pem"
    cert = staging / CERT_PATH.name
    ca = staging / CA_PATH.name
    try:
        run_command(["openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(key)])
        os.chown(key, 65532, 65532)
        os.chmod(key, 0o600)
        run_command(
            [
                "openssl",
                "req",
                "-new",
                "-sha256",
                "-key",
                str(key),
                "-out",
                str(csr),
                "-subj",
                "/CN=orion-workload",
            ]
        )

        binding = {
            "workload": WORKLOAD,
            "environment": ENVIRONMENT,
            "instance_id": INSTANCE_ID,
            "node_id": NODE_ID,
            "agent_id": AGENT_ID,
        }
        challenge = agent_post("api/v1/workload/challenges", binding)
        certificate = agent_post(
            "api/v1/workload/certificates",
            {"challenge": challenge["challenge"], "csr_pem": csr.read_text(encoding="ascii")},
        )
        if certificate.get("spiffe_id") != EXPECTED_SAN:
            raise RotationError("certificate_identity_mismatch")
        if (
            certificate.get("workload") != WORKLOAD
            or certificate.get("environment") != ENVIRONMENT
            or certificate.get("instance_id") != INSTANCE_ID
        ):
            raise RotationError("certificate_binding_mismatch")

        write_public_file(cert, certificate["certificate_pem"])
        write_public_file(ca, certificate["ca_certificate_pem"])
        validate_material(cert, key)

        had_previous = hardlink_previous(previous)
        os.replace(cert, CERT_PATH)
        os.replace(key, KEY_PATH)
        os.replace(ca, CA_PATH)
        try:
            docker_restart_and_wait()
        except RotationError:
            if had_previous:
                restore_previous(previous)
                docker_restart_and_wait()
            raise
        log("rotation", status="installed", identity=EXPECTED_SAN, expires_at=certificate.get("expires_at"))
    finally:
        shutil.rmtree(staging, ignore_errors=True)


def main() -> int:
    LOCK_PATH.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    with LOCK_PATH.open("a+") as lock:
        try:
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            log("rotation", status="skipped", reason="already_running")
            return 0

        if cert_is_valid_for(ROTATE_BEFORE_SECONDS):
            log("rotation", status="not_needed", identity=EXPECTED_SAN)
            return 0
        try:
            rotate()
        except (OSError, RotationError, KeyError, TypeError) as exc:
            log("rotation", status="failed", reason=str(exc))
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
