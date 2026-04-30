"""EventSub WebSocket supervisor.

Maintains one outbound WS connection per credential to
``wss://eventsub.wss.twitch.tv/ws``. The Twitch flow :

1. Connect → receive ``session_welcome`` carrying ``session.id``.
2. Use that ``session_id`` when creating ``/eventsub/subscriptions``
   via Helix — Twitch then pushes events on the same WS.
3. Handle ``session_keepalive`` (no-op), ``notification`` (event
   delivery), ``revocation`` (subscription lost), and ``session_reconnect``
   (Twitch is rotating the WS — connect to the new URL, then drop the
   old).
4. Reconnect on disconnect with exponential backoff. Existing
   subscriptions auto-migrate when we re-create them on the new
   ``session_id``.

Like the IRC supervisor, this module exposes :data:`_factory` and a
data-access seam so unit tests can stand in stubs and never touch
real Twitch.

This is the **scaffolding** delivery — it covers the WS lifecycle and
notification fan-out into :data:`eventsub_bus`. Auto-creating the
subscriptions catalogued in ``eventsub_subscriptions`` on connect is
left to a follow-up so this PR stays reviewable.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
from collections.abc import AsyncIterator, Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any

from sqlalchemy.exc import IntegrityError

from orion.database import async_session
from orion.models.eventsub import EventSubEvent
from orion.services.eventsub_bus import eventsub_bus

logger = logging.getLogger(__name__)

EVENTSUB_WS_URL = "wss://eventsub.wss.twitch.tv/ws"
RECONNECT_BASE_SECONDS = 1.0
RECONNECT_MAX_SECONDS = 60.0


@dataclass
class _Connection:
    credential_id: str
    session_id: str | None = None
    pump: asyncio.Task[None] | None = None
    closing: asyncio.Event = field(default_factory=asyncio.Event)


# ── WS client abstraction ──────────────────────────────────────────────────


class WebSocketLike:
    """Protocol-ish interface : ``async for`` yields decoded JSON
    payloads ; ``close()`` shuts the underlying connection. The real
    implementation is :class:`websockets.WebSocketClientProtocol` ;
    tests inject their own."""

    async def __aenter__(self) -> WebSocketLike:
        raise NotImplementedError

    async def __aexit__(self, *exc: object) -> None:
        raise NotImplementedError

    def __aiter__(self) -> AsyncIterator[dict[str, Any]]:
        raise NotImplementedError

    async def close(self) -> None:
        raise NotImplementedError


WebSocketFactory = Callable[[str], WebSocketLike]


def _default_factory(url: str) -> WebSocketLike:
    """Real websockets-based implementation. Imported lazily so tests
    can run without the ``websockets`` package being importable in
    every environment."""
    import websockets  # noqa: PLC0415 — defer import

    class _RealClient(WebSocketLike):
        def __init__(self, target: str) -> None:
            self._target = target
            self._conn: Any = None

        async def __aenter__(self) -> _RealClient:
            self._conn = await websockets.connect(self._target).__aenter__()
            return self

        async def __aexit__(self, *exc: object) -> None:
            if self._conn is not None:
                await self._conn.__aexit__(*exc)

        def __aiter__(self) -> AsyncIterator[dict[str, Any]]:
            return self._iter()

        async def _iter(self) -> AsyncIterator[dict[str, Any]]:
            assert self._conn is not None
            async for msg in self._conn:
                if isinstance(msg, bytes):
                    msg = msg.decode("utf-8", errors="replace")
                try:
                    yield json.loads(msg)
                except json.JSONDecodeError:
                    logger.warning("eventsub: dropping non-JSON frame: %r", msg[:200])

        async def close(self) -> None:
            if self._conn is not None:
                await self._conn.close()

    return _RealClient(url)


# ── Supervisor ─────────────────────────────────────────────────────────────


class EventSubSupervisor:
    """One WS connection per credential. Reconnects with backoff. Pushes
    every accepted notification into :data:`eventsub_bus` and persists
    it to ``eventsub_events`` for audit + dedup."""

    def __init__(self) -> None:
        self._connections: dict[str, _Connection] = {}
        self._global_lock = asyncio.Lock()
        self._factory: WebSocketFactory = _default_factory
        self._url: str = EVENTSUB_WS_URL

    # ── Test seams ────────────────────────────────────────────────────

    def set_websocket_factory(self, factory: WebSocketFactory) -> None:
        self._factory = factory

    def set_url(self, url: str) -> None:
        self._url = url

    # ── Public API ────────────────────────────────────────────────────

    async def open(self, credential_id: str) -> None:
        """Start (or no-op if running) the WS pump for a credential."""
        key = credential_id
        async with self._global_lock:
            if key in self._connections:
                return
            conn = _Connection(credential_id=key)
            self._connections[key] = conn
            conn.pump = asyncio.create_task(
                self._run(conn),
                name=f"orion.eventsub_pump:{key[:8]}",
            )

    async def close(self, credential_id: str) -> None:
        async with self._global_lock:
            conn = self._connections.pop(credential_id, None)
        if conn is None:
            return
        conn.closing.set()
        if conn.pump is not None:
            conn.pump.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await conn.pump

    async def shutdown(self) -> None:
        for credential_id in list(self._connections.keys()):
            await self.close(credential_id)

    def session_id(self, credential_id: str) -> str | None:
        conn = self._connections.get(credential_id)
        return conn.session_id if conn is not None else None

    async def wait_for_session(self, credential_id: str, timeout: float = 10.0) -> str:
        """Block until ``session_welcome`` has set ``session_id`` for the
        credential. Raises :class:`asyncio.TimeoutError` if Twitch
        doesn't say hello in time. Caller is responsible for having
        called :meth:`open` first."""
        deadline = asyncio.get_event_loop().time() + timeout
        while True:
            conn = self._connections.get(credential_id)
            if conn is not None and conn.session_id:
                return conn.session_id
            if asyncio.get_event_loop().time() >= deadline:
                raise TimeoutError(
                    f"eventsub: no session_welcome for credential {credential_id} "
                    f"within {timeout}s"
                )
            await asyncio.sleep(0.05)

    # ── Internals ─────────────────────────────────────────────────────

    async def _run(self, conn: _Connection) -> None:
        attempt = 0
        while not conn.closing.is_set():
            try:
                await self._connect_and_pump(conn)
                attempt = 0
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception(
                    "eventsub: pump for credential %s crashed", conn.credential_id
                )
            if conn.closing.is_set():
                return
            delay = min(RECONNECT_MAX_SECONDS, RECONNECT_BASE_SECONDS * (2**attempt))
            attempt += 1
            try:
                await asyncio.wait_for(conn.closing.wait(), timeout=delay)
                return
            except TimeoutError:
                continue

    async def _connect_and_pump(self, conn: _Connection) -> None:
        ws = self._factory(self._url)
        async with ws as live:
            async for frame in live:
                metadata = frame.get("metadata", {})
                payload = frame.get("payload", {})
                msg_type = metadata.get("message_type")
                if msg_type == "session_welcome":
                    session = payload.get("session", {})
                    conn.session_id = session.get("id")
                    logger.info(
                        "eventsub: welcome on credential %s, session_id=%s",
                        conn.credential_id,
                        conn.session_id,
                    )
                elif msg_type == "session_keepalive":
                    continue
                elif msg_type == "notification":
                    await self._handle_notification(metadata, payload)
                elif msg_type == "session_reconnect":
                    new_url = (payload.get("session") or {}).get("reconnect_url")
                    logger.info(
                        "eventsub: reconnect requested for credential %s",
                        conn.credential_id,
                    )
                    if new_url:
                        # Switch URL for the next iteration of the
                        # outer reconnect loop.
                        self._url = new_url
                    return
                elif msg_type == "revocation":
                    sub = payload.get("subscription", {})
                    logger.warning(
                        "eventsub: revoked subscription %s (status=%s)",
                        sub.get("id"),
                        sub.get("status"),
                    )
                else:
                    logger.debug("eventsub: ignoring message_type=%r", msg_type)

    async def _handle_notification(
        self, metadata: dict[str, Any], payload: dict[str, Any]
    ) -> None:
        message_id = metadata.get("message_id")
        subscription = payload.get("subscription") or {}
        event = payload.get("event") or {}
        event_type = subscription.get("type") or metadata.get("subscription_type") or "unknown"
        if not message_id:
            logger.warning("eventsub: notification with no message_id, dropping")
            return

        # Dedup via the unique twitch_message_id index. IntegrityError
        # is the expected outcome when Twitch replays — swallow it.
        async with async_session() as session:
            session.add(
                EventSubEvent(
                    twitch_message_id=str(message_id),
                    subscription_id=None,  # mapping to local row is a follow-up
                    event_type=event_type,
                    event=event,
                    received_at=datetime.now(tz=UTC),
                )
            )
            try:
                await session.commit()
            except IntegrityError:
                await session.rollback()
                return

        broadcaster_id = (
            event.get("broadcaster_user_id")
            or subscription.get("condition", {}).get("broadcaster_user_id")
        )
        if broadcaster_id:
            await eventsub_bus.publish(
                str(broadcaster_id),
                {
                    "event_type": event_type,
                    "event": event,
                    "subscription": subscription,
                    "received_at": datetime.now(tz=UTC).isoformat(),
                },
            )


supervisor = EventSubSupervisor()
