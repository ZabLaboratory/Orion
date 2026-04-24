"""AES-GCM envelope for at-rest secrets (Twitch stream keys, OAuth tokens).

Encryption key is loaded once from ``settings.encryption_key`` (urlsafe-b64, 32 bytes).
We store ciphertext + nonce as separate BYTEA columns; the tag is appended to the
ciphertext by the ``AESGCM`` primitive.
"""

from __future__ import annotations

import base64
import os

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

from orion.config import settings

_NONCE_BYTES = 12


class EncryptionNotConfigured(RuntimeError):
    """Raised when encryption is requested but no key is configured."""


def _load_key() -> bytes:
    raw = settings.encryption_key
    if not raw:
        raise EncryptionNotConfigured(
            "ENCRYPTION_KEY is empty. Generate one with: "
            "python -c \"import secrets,base64; print(base64.urlsafe_b64encode(secrets.token_bytes(32)).decode())\""
        )
    key = base64.urlsafe_b64decode(raw.encode())
    if len(key) != 32:
        raise EncryptionNotConfigured(
            f"ENCRYPTION_KEY must decode to exactly 32 bytes, got {len(key)} bytes."
        )
    return key


def encrypt(plaintext: str) -> tuple[bytes, bytes]:
    """Encrypt a UTF-8 string. Returns ``(ciphertext, nonce)`` — store both columns."""
    key = _load_key()
    nonce = os.urandom(_NONCE_BYTES)
    aes = AESGCM(key)
    ct = aes.encrypt(nonce, plaintext.encode("utf-8"), associated_data=None)
    return ct, nonce


def decrypt(ciphertext: bytes, nonce: bytes) -> str:
    """Reverse of ``encrypt``. Raises ``cryptography.exceptions.InvalidTag`` on tamper."""
    key = _load_key()
    aes = AESGCM(key)
    pt = aes.decrypt(nonce, ciphertext, associated_data=None)
    return pt.decode("utf-8")
