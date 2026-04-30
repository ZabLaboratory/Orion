"""Helix endpoint surface — channel info, schedule, clips, predictions.

Tests stub :class:`twitch_helix.HelixClient` so the suite never touches
the real Twitch API. A few cases drive the refresh-on-401 path.
"""

from __future__ import annotations

import uuid
from typing import Any

import pytest
from httpx import AsyncClient

from orion.database import async_session
from orion.models.credential import TwitchCredential
from orion.services import encryption, twitch_helix

# A 32-byte urlsafe-b64 key matching the test ENCRYPTION_KEY in conftest.
TEST_TOKEN = "user-access-token"
TEST_REFRESH = "user-refresh-token"
USER_ID = uuid.UUID("11111111-1111-1111-1111-111111111111")


# ── Fakes ──────────────────────────────────────────────────────────────────


class FakeHelixClient:
    """Captures every call so tests can assert behaviour. Knobs:

    - ``raise_auth_first`` makes the first call to any method raise
      :class:`TwitchAuthError` ; the second succeeds. Drives the
      refresh-and-retry path.
    """

    instances: list[FakeHelixClient] = []

    def __init__(self, access_token: str) -> None:
        self.access_token = access_token
        self.calls: list[tuple[str, tuple[Any, ...], dict[str, Any]]] = []
        self.closed = False
        FakeHelixClient.instances.append(self)

    async def aclose(self) -> None:
        self.closed = True

    async def get_channel(self, broadcaster_id: str) -> dict[str, Any] | None:
        self.calls.append(("get_channel", (broadcaster_id,), {}))
        if FakeHelixClient._next_call_should_401(self):
            raise twitch_helix.TwitchAuthError("test 401")
        return {"broadcaster_id": broadcaster_id, "title": "Test stream"}

    async def update_channel(self, broadcaster_id: str, **kwargs: Any) -> None:
        self.calls.append(("update_channel", (broadcaster_id,), kwargs))
        if FakeHelixClient._next_call_should_401(self):
            raise twitch_helix.TwitchAuthError("test 401")

    async def get_schedule(self, broadcaster_id: str, *, first: int = 25) -> dict[str, Any]:
        self.calls.append(("get_schedule", (broadcaster_id,), {"first": first}))
        return {"data": {"broadcaster_id": broadcaster_id, "segments": []}, "pagination": {}}

    async def get_clips(self, broadcaster_id: str, *, first: int = 20) -> dict[str, Any]:
        self.calls.append(("get_clips", (broadcaster_id,), {"first": first}))
        return {"data": [], "pagination": {}}

    async def get_predictions(self, broadcaster_id: str, *, first: int = 25) -> dict[str, Any]:
        self.calls.append(("get_predictions", (broadcaster_id,), {"first": first}))
        return {"data": [], "pagination": {}}

    # internal — first call hits 401, subsequent calls succeed
    @classmethod
    def _next_call_should_401(cls, instance: FakeHelixClient) -> bool:
        return getattr(instance, "_should_401", False) and not getattr(instance, "_already_401ed", False)


@pytest.fixture(autouse=True)
def _reset_helix_clients() -> None:
    FakeHelixClient.instances.clear()


@pytest.fixture(autouse=True)
def _patch_helix_client(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(twitch_helix, "HelixClient", FakeHelixClient)


# ── Credential setup ──────────────────────────────────────────────────────


async def _seed_credential(*, with_oauth: bool, with_channel_id: bool = True) -> uuid.UUID:
    sk_ct, sk_nonce = encryption.encrypt("stream-key")
    cred = TwitchCredential(
        owner_id=USER_ID,
        label="test",
        channel_login="testchannel",
        channel_id="123456" if with_channel_id else None,
        stream_key_ciphertext=sk_ct,
        stream_key_nonce=sk_nonce,
    )
    if with_oauth:
        access_ct, access_nonce = encryption.encrypt(TEST_TOKEN)
        refresh_ct, refresh_nonce = encryption.encrypt(TEST_REFRESH)
        cred.oauth_access_ciphertext = access_ct
        cred.oauth_access_nonce = access_nonce
        cred.oauth_refresh_ciphertext = refresh_ct
        cred.oauth_refresh_nonce = refresh_nonce
        cred.oauth_scopes = ["channel:manage:broadcast"]
    async with async_session() as session:
        session.add(cred)
        await session.commit()
        # Don't refresh — SQLite + the PG-specific UUID type round-trip is
        # quirky for stale ORM objects, and we only need cred.id which is
        # already set by the Python default before the INSERT.
        return cred.id


# ── GET channel ───────────────────────────────────────────────────────────


async def test_get_channel_happy_path(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=True)
    resp = await auth_client.get(f"/api/v1/twitch/credentials/{cred_id}/channel")
    assert resp.status_code == 200, resp.text
    assert resp.json()["title"] == "Test stream"
    assert resp.json()["broadcaster_id"] == "123456"
    assert FakeHelixClient.instances[0].closed is True


async def test_get_channel_requires_oauth(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=False)
    resp = await auth_client.get(f"/api/v1/twitch/credentials/{cred_id}/channel")
    assert resp.status_code == 412
    assert "OAuth" in resp.json()["detail"]


async def test_get_channel_requires_channel_id(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=True, with_channel_id=False)
    resp = await auth_client.get(f"/api/v1/twitch/credentials/{cred_id}/channel")
    assert resp.status_code == 412
    assert "channel_id" in resp.json()["detail"]


async def test_get_channel_404_on_foreign_credential(client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=True)
    other_user = "22222222-2222-2222-2222-222222222222"
    resp = await client.get(
        f"/api/v1/twitch/credentials/{cred_id}/channel",
        headers={"X-Authenticated-User": other_user},
    )
    assert resp.status_code == 404


async def test_get_channel_unauth_returns_401(client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=True)
    resp = await client.get(f"/api/v1/twitch/credentials/{cred_id}/channel")
    assert resp.status_code == 401


# ── PATCH channel ─────────────────────────────────────────────────────────


async def test_patch_channel_forwards_payload(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=True)
    resp = await auth_client.patch(
        f"/api/v1/twitch/credentials/{cred_id}/channel",
        json={"title": "Going live!", "game_id": "1234", "tags": ["zab", "live"]},
    )
    assert resp.status_code == 204, resp.text
    method, args, kwargs = FakeHelixClient.instances[0].calls[0]
    assert method == "update_channel"
    assert args == ("123456",)
    assert kwargs["title"] == "Going live!"
    assert kwargs["game_id"] == "1234"
    assert kwargs["tags"] == ["zab", "live"]


async def test_patch_channel_partial(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=True)
    resp = await auth_client.patch(
        f"/api/v1/twitch/credentials/{cred_id}/channel",
        json={"title": "Just a title bump"},
    )
    assert resp.status_code == 204
    method, _args, kwargs = FakeHelixClient.instances[0].calls[0]
    assert kwargs["title"] == "Just a title bump"
    assert kwargs["game_id"] is None
    assert kwargs["tags"] is None


# ── GET schedule / clips / predictions ────────────────────────────────────


async def test_get_schedule_returns_payload(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=True)
    resp = await auth_client.get(f"/api/v1/twitch/credentials/{cred_id}/schedule")
    assert resp.status_code == 200
    body = resp.json()
    assert body["data"]["broadcaster_id"] == "123456"
    assert body["data"]["segments"] == []


async def test_get_clips_returns_payload(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=True)
    resp = await auth_client.get(
        f"/api/v1/twitch/credentials/{cred_id}/clips?first=10"
    )
    assert resp.status_code == 200
    method, _args, kwargs = FakeHelixClient.instances[0].calls[0]
    assert method == "get_clips"
    assert kwargs == {"first": 10}


async def test_get_predictions_returns_payload(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential(with_oauth=True)
    resp = await auth_client.get(f"/api/v1/twitch/credentials/{cred_id}/predictions")
    assert resp.status_code == 200
    assert FakeHelixClient.instances[0].calls[0][0] == "get_predictions"


# ── Refresh-on-401 ────────────────────────────────────────────────────────


async def test_get_channel_refreshes_on_401(
    auth_client: AsyncClient,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    cred_id = await _seed_credential(with_oauth=True)

    # Arm the first FakeHelixClient instance to 401 once. The session
    # helper closes it, refreshes, and constructs a second client.
    original_init = FakeHelixClient.__init__

    def init_with_arming(self: FakeHelixClient, access_token: str) -> None:
        original_init(self, access_token)
        if len(FakeHelixClient.instances) == 1:
            self._should_401 = True  # type: ignore[attr-defined]
        # mark already-401ed BEFORE the call so only the first attempt raises
        else:
            self._already_401ed = True  # type: ignore[attr-defined]

    monkeypatch.setattr(FakeHelixClient, "__init__", init_with_arming)

    # Stub the refresh call to return a new token.
    async def fake_refresh(refresh: str) -> dict[str, Any]:
        assert refresh == TEST_REFRESH
        return {
            "access_token": "refreshed-access-token",
            "refresh_token": "new-refresh-token",
            "expires_in": 3600,
            "scope": ["channel:manage:broadcast"],
        }

    monkeypatch.setattr(twitch_helix, "refresh_token", fake_refresh)

    resp = await auth_client.get(f"/api/v1/twitch/credentials/{cred_id}/channel")
    assert resp.status_code == 200, resp.text
    # Two clients constructed : the first that 401'd, the second after refresh.
    assert len(FakeHelixClient.instances) == 2
    assert FakeHelixClient.instances[0].access_token == TEST_TOKEN
    assert FakeHelixClient.instances[1].access_token == "refreshed-access-token"
    # Both clients are closed.
    assert all(c.closed for c in FakeHelixClient.instances)
