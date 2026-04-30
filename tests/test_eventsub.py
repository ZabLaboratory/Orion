"""EventSub — supervisor + subscription CRUD + live fan-out.

Tests stub the WebSocket and the Helix client so the suite never
opens a real outbound connection. The supervisor is exercised at
the unit level (welcome / notification / dedup) and via the create
route end-to-end.
"""

from __future__ import annotations

import asyncio
import uuid
from collections.abc import AsyncIterator
from typing import Any

import pytest
from httpx import AsyncClient
from sqlalchemy import select

from orion.database import async_session
from orion.models.credential import TwitchCredential
from orion.models.eventsub import EventSubEvent, EventSubSubscription
from orion.services import encryption, twitch_helix
from orion.services.eventsub_bus import eventsub_bus
from orion.services.eventsub_supervisor import EventSubSupervisor

USER_ID = uuid.UUID("11111111-1111-1111-1111-111111111111")


# ── Fake WebSocket ─────────────────────────────────────────────────────────


class FakeWebSocket:
    """Hand-fed JSON frame stream. Tests push frames via :meth:`feed`
    then ``feed(None)`` to close cleanly."""

    instances: list[FakeWebSocket] = []

    def __init__(self, url: str) -> None:
        self.url = url
        self._inbox: asyncio.Queue[dict[str, Any] | None] = asyncio.Queue()
        self.closed = False
        FakeWebSocket.instances.append(self)

    async def __aenter__(self) -> FakeWebSocket:
        return self

    async def __aexit__(self, *exc: object) -> None:
        self.closed = True

    def __aiter__(self) -> AsyncIterator[dict[str, Any]]:
        return self._iter()

    async def _iter(self) -> AsyncIterator[dict[str, Any]]:
        while True:
            item = await self._inbox.get()
            if item is None:
                return
            yield item

    async def feed(self, frame: dict[str, Any] | None) -> None:
        await self._inbox.put(frame)

    async def close(self) -> None:
        self.closed = True
        await self._inbox.put(None)


@pytest.fixture(autouse=True)
def _reset_fakes() -> None:
    FakeWebSocket.instances.clear()


# ── Unit : supervisor lifecycle ────────────────────────────────────────────


@pytest.fixture
def supervisor() -> EventSubSupervisor:
    sup = EventSubSupervisor()
    sup.set_websocket_factory(FakeWebSocket)  # type: ignore[arg-type]
    sup.set_url("ws://test")
    return sup


async def _wait_for_ws(timeout: float = 1.0) -> FakeWebSocket:
    deadline = asyncio.get_event_loop().time() + timeout
    while asyncio.get_event_loop().time() < deadline:
        if FakeWebSocket.instances:
            return FakeWebSocket.instances[-1]
        await asyncio.sleep(0.01)
    raise AssertionError("FakeWebSocket never opened")


async def test_open_and_session_welcome(supervisor: EventSubSupervisor) -> None:
    await supervisor.open("cred-1")
    ws = await _wait_for_ws()
    await ws.feed(
        {
            "metadata": {"message_type": "session_welcome"},
            "payload": {"session": {"id": "session-abc"}},
        }
    )
    sid = await supervisor.wait_for_session("cred-1", timeout=2.0)
    assert sid == "session-abc"
    await supervisor.shutdown()


async def test_notification_dedup_and_publish(
    supervisor: EventSubSupervisor,
) -> None:
    """Same Twitch ``message_id`` shows up twice → only one row, only
    one publish. Ensures the unique index handles replays cleanly."""
    await supervisor.open("cred-1")
    ws = await _wait_for_ws()
    await ws.feed(
        {
            "metadata": {"message_type": "session_welcome"},
            "payload": {"session": {"id": "session-abc"}},
        }
    )
    await supervisor.wait_for_session("cred-1", timeout=2.0)

    queue = eventsub_bus.subscribe("99999")
    try:
        for _ in range(2):
            await FakeWebSocket.instances[-1].feed(
                {
                    "metadata": {
                        "message_type": "notification",
                        "message_id": "msg-1",
                    },
                    "payload": {
                        "subscription": {"type": "channel.subscribe", "id": "sub-x"},
                        "event": {"broadcaster_user_id": "99999", "user_login": "alice"},
                    },
                }
            )
        # Wait on the bus first — the pump publishes AFTER committing.
        # If we get an event back the row has landed.
        first = await asyncio.wait_for(queue.get(), timeout=2.0)
        assert first["event_type"] == "channel.subscribe"
        # Give the second feed a chance to be dropped on the unique
        # index and then assert the dedup state.
        for _ in range(20):
            await asyncio.sleep(0.01)
        async with async_session() as session:
            stmt = select(EventSubEvent).where(EventSubEvent.twitch_message_id == "msg-1")
            rows = (await session.execute(stmt)).scalars().all()
            assert len(rows) == 1
        assert queue.empty()
    finally:
        eventsub_bus.unsubscribe("99999", queue)
        await supervisor.shutdown()


async def test_keepalive_and_unknown_messages_ignored(
    supervisor: EventSubSupervisor,
) -> None:
    await supervisor.open("cred-1")
    ws = await _wait_for_ws()
    await ws.feed(
        {
            "metadata": {"message_type": "session_welcome"},
            "payload": {"session": {"id": "session-abc"}},
        }
    )
    await ws.feed({"metadata": {"message_type": "session_keepalive"}, "payload": {}})
    await ws.feed({"metadata": {"message_type": "weird_thing"}, "payload": {}})
    await ws.feed(None)  # close cleanly
    # Pump exits, supervisor goes into reconnect-loop ; just shut it down.
    await supervisor.shutdown()


# ── Integration : create subscription via route ────────────────────────────


class FakeHelixClient:
    instances: list[FakeHelixClient] = []

    def __init__(self, access_token: str) -> None:
        self.access_token = access_token
        self.calls: list[tuple[str, dict[str, Any]]] = []
        self.closed = False
        FakeHelixClient.instances.append(self)

    async def aclose(self) -> None:
        self.closed = True

    async def create_eventsub_subscription(
        self,
        *,
        event_type: str,
        version: str,
        condition: dict[str, Any],
        session_id: str,
    ) -> dict[str, Any]:
        self.calls.append(
            (
                "create",
                {
                    "event_type": event_type,
                    "version": version,
                    "condition": condition,
                    "session_id": session_id,
                },
            )
        )
        return {
            "id": "twitch-sub-id-123",
            "type": event_type,
            "version": version,
            "status": "enabled",
            "cost": 1,
            "condition": condition,
        }

    async def delete_eventsub_subscription(self, subscription_id: str) -> None:
        self.calls.append(("delete", {"id": subscription_id}))


@pytest.fixture(autouse=True)
def _reset_helix() -> None:
    FakeHelixClient.instances.clear()


async def _seed_credential() -> uuid.UUID:
    sk_ct, sk_nonce = encryption.encrypt("stream-key")
    access_ct, access_nonce = encryption.encrypt("access-token")
    cred = TwitchCredential(
        owner_id=USER_ID,
        label="test",
        channel_login="alice",
        channel_id="99999",
        stream_key_ciphertext=sk_ct,
        stream_key_nonce=sk_nonce,
        oauth_access_ciphertext=access_ct,
        oauth_access_nonce=access_nonce,
        oauth_scopes=["channel:read:subscriptions"],
    )
    async with async_session() as session:
        session.add(cred)
        await session.commit()
        return cred.id


async def test_create_subscription_end_to_end(
    auth_client: AsyncClient, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(twitch_helix, "HelixClient", FakeHelixClient)

    from orion.routes import eventsub as routes
    from orion.services import eventsub_supervisor as sup_module

    test_sup = EventSubSupervisor()
    test_sup.set_websocket_factory(FakeWebSocket)  # type: ignore[arg-type]
    test_sup.set_url("ws://test")
    monkeypatch.setattr(routes, "eventsub_supervisor", test_sup)
    monkeypatch.setattr(sup_module, "supervisor", test_sup)

    cred_id = await _seed_credential()

    async def feed_welcome() -> None:
        # Wait for the WS to be created by the route handler, then feed.
        for _ in range(50):
            if FakeWebSocket.instances:
                break
            await asyncio.sleep(0.01)
        await FakeWebSocket.instances[0].feed(
            {
                "metadata": {"message_type": "session_welcome"},
                "payload": {"session": {"id": "session-xyz"}},
            }
        )

    feed_task = asyncio.create_task(feed_welcome())
    try:
        resp = await auth_client.post(
            f"/api/v1/eventsub/credentials/{cred_id}/subscriptions",
            json={
                "event_type": "channel.subscribe",
                "version": "1",
                "condition": {"broadcaster_user_id": "99999"},
            },
        )
    finally:
        await feed_task
        await test_sup.shutdown()

    assert resp.status_code == 201, resp.text
    body = resp.json()
    assert body["twitch_subscription_id"] == "twitch-sub-id-123"
    assert body["event_type"] == "channel.subscribe"
    assert body["status"] == "enabled"
    # Helix received our session_id.
    assert FakeHelixClient.instances[0].calls[0] == (
        "create",
        {
            "event_type": "channel.subscribe",
            "version": "1",
            "condition": {"broadcaster_user_id": "99999"},
            "session_id": "session-xyz",
        },
    )


async def test_list_subscriptions_returns_local_rows(auth_client: AsyncClient) -> None:
    cred_id = await _seed_credential()
    async with async_session() as session:
        session.add(
            EventSubSubscription(
                credential_id=cred_id,
                twitch_subscription_id="t1",
                event_type="channel.cheer",
                version="1",
                status="enabled",
                cost=1,
                condition={"broadcaster_user_id": "99999"},
            )
        )
        await session.commit()
    resp = await auth_client.get(
        f"/api/v1/eventsub/credentials/{cred_id}/subscriptions"
    )
    assert resp.status_code == 200
    body = resp.json()
    assert len(body) == 1
    assert body[0]["event_type"] == "channel.cheer"


async def test_delete_subscription_calls_helix(
    auth_client: AsyncClient, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(twitch_helix, "HelixClient", FakeHelixClient)
    cred_id = await _seed_credential()

    async with async_session() as session:
        sub = EventSubSubscription(
            credential_id=cred_id,
            twitch_subscription_id="twitch-sub-doomed",
            event_type="channel.follow",
            version="2",
            status="enabled",
            cost=1,
            condition={"broadcaster_user_id": "99999", "moderator_user_id": "99999"},
        )
        session.add(sub)
        await session.commit()
        sub_id = sub.id

    resp = await auth_client.delete(f"/api/v1/eventsub/subscriptions/{sub_id}")
    assert resp.status_code == 204
    assert FakeHelixClient.instances[-1].calls[-1] == (
        "delete",
        {"id": "twitch-sub-doomed"},
    )

    # Local row gone.
    async with async_session() as session:
        result = await session.get(EventSubSubscription, sub_id)
        assert result is None


async def test_create_subscription_404_on_foreign_credential(
    client: AsyncClient,
) -> None:
    cred_id = await _seed_credential()
    other = "22222222-2222-2222-2222-222222222222"
    resp = await client.post(
        f"/api/v1/eventsub/credentials/{cred_id}/subscriptions",
        json={"event_type": "channel.subscribe", "condition": {}},
        headers={"X-Authenticated-User": other},
    )
    assert resp.status_code == 404


async def test_create_subscription_unauth(client: AsyncClient) -> None:
    cred_id = await _seed_credential()
    resp = await client.post(
        f"/api/v1/eventsub/credentials/{cred_id}/subscriptions",
        json={"event_type": "channel.subscribe", "condition": {}},
    )
    assert resp.status_code == 401
