"""Credential CRUD + the decrypted stream-key endpoint.

The stream-key endpoint is the one exception to "secrets never leave
the server" — Pulsar (broadcast engine in Prism) needs the plaintext
to push RTMP. Tests assert owner-scoping and auth.
"""

from __future__ import annotations

import uuid

from httpx import AsyncClient

from orion.database import async_session
from orion.models.credential import TwitchCredential
from orion.services import encryption

USER_ID = uuid.UUID("11111111-1111-1111-1111-111111111111")


async def _seed_credential(*, owner_id: uuid.UUID = USER_ID, key: str = "live_secret_xyz") -> uuid.UUID:
    sk_ct, sk_nonce = encryption.encrypt(key)
    cred = TwitchCredential(
        owner_id=owner_id,
        label="test",
        channel_login="testchannel",
        channel_id="123456",
        stream_key_ciphertext=sk_ct,
        stream_key_nonce=sk_nonce,
    )
    async with async_session() as session:
        session.add(cred)
        await session.commit()
        return cred.id


async def test_get_stream_key_returns_decrypted(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential(key="live_supersecret_42")
    resp = await auth_client.get(f"/api/v1/credentials/{cred_id}/stream-key")
    assert resp.status_code == 200, resp.text
    assert resp.json() == {"stream_key": "live_supersecret_42"}


async def test_get_stream_key_unauth_falls_back_to_404(client: AsyncClient) -> None:
    """Anonymous callers never see a credential they don't own. The
    endpoint matches the rest of the credentials surface (permissive
    auth + ownership check) so the scaffolding-mode flow still works
    when the gateway hasn't injected X-Authenticated-User yet ; an
    owned credential just doesn't match a NULL caller."""
    cred_id = await _seed_credential()  # owner_id = USER_ID
    resp = await client.get(f"/api/v1/credentials/{cred_id}/stream-key")
    assert resp.status_code == 404


async def test_get_stream_key_404_on_foreign_credential(client: AsyncClient) -> None:
    cred_id = await _seed_credential()
    other_user = "22222222-2222-2222-2222-222222222222"
    resp = await client.get(
        f"/api/v1/credentials/{cred_id}/stream-key",
        headers={"X-Authenticated-User": other_user},
    )
    assert resp.status_code == 404


async def test_get_stream_key_404_on_unknown_credential(auth_client: AsyncClient) -> None:
    resp = await auth_client.get(
        f"/api/v1/credentials/{uuid.uuid4()}/stream-key"
    )
    assert resp.status_code == 404


async def test_credential_list_does_not_leak_stream_key(auth_client: AsyncClient) -> None:
    """The list endpoint stays restrictive — `has_oauth: bool` only,
    no ciphertext, no key. Regression guard against accidental
    serialiser changes."""
    await _seed_credential(key="live_should_not_leak")
    resp = await auth_client.get("/api/v1/credentials")
    assert resp.status_code == 200
    body = resp.json()
    assert len(body) == 1
    item = body[0]
    assert "stream_key" not in item
    assert "stream_key_ciphertext" not in item
    assert "stream_key_nonce" not in item
    assert item.get("has_oauth") is False
