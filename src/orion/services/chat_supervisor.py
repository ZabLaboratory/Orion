"""Twitch IRC chat supervisor.

Runs as a single asyncio task inside the FastAPI lifespan. Every ``POLL_INTERVAL``
seconds it reconciles its in-memory map of connected IRC clients against the DB:

- For every stream in ``LIVE`` state whose credential has a valid OAuth access
  token with chat scopes AND a ``channel_login`` set, ensure one IRC client is
  connected and joined to that channel.
- For every stream that has left the ``LIVE`` state, tear down its client.

Each IRC client has its own task that reads ``TwitchChatClient.messages()`` and
pipes decoded payloads into ``chat_bus`` (for WS subscribers) and into
``chat_messages`` (for replay / audit).
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime

from sqlalchemy import select

from orion.database import async_session
from orion.models.chat import ChatMessage
from orion.models.credential import TwitchCredential
from orion.models.stream import Stream, StreamState
from orion.services import credential_service
from orion.services.chat_service import chat_bus
from orion.services.twitch_chat import ChatMessage as IrcMessage
from orion.services.twitch_chat import TwitchChatClient

logger = logging.getLogger(__name__)

POLL_INTERVAL_SECONDS = 10
_REQUIRED_SCOPES = ("chat:read",)


@dataclass
class _Connection:
    stream_id: uuid.UUID
    channel: str
    client: TwitchChatClient
    pump: asyncio.Task[None]


class ChatSupervisor:
    """Owns the IRC fleet for the lifetime of the Orion process.

    There is exactly one supervisor. It is not re-entrant and must be started /
    stopped by the FastAPI lifespan.
    """

    def __init__(self) -> None:
        self._connections: dict[uuid.UUID, _Connection] = {}
        self._task: asyncio.Task[None] | None = None
        self._stopping = asyncio.Event()

    async def start(self) -> None:
        if self._task is not None:
            return
        self._stopping.clear()
        self._task = asyncio.create_task(self._run(), name="orion.chat_supervisor")

    async def stop(self) -> None:
        self._stopping.set()
        if self._task is not None:
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task
            self._task = None
        # Tear down remaining connections sequentially — cheap, bounded fleet.
        for conn in list(self._connections.values()):
            await self._drop(conn)

    async def _run(self) -> None:
        logger.info("chat supervisor running (interval=%ss)", POLL_INTERVAL_SECONDS)
        while not self._stopping.is_set():
            try:
                await self._reconcile()
            except Exception:
                logger.exception("chat supervisor reconcile tick failed")
            try:
                await asyncio.wait_for(self._stopping.wait(), timeout=POLL_INTERVAL_SECONDS)
            except TimeoutError:
                continue

    async def _reconcile(self) -> None:
        """Compare desired state (DB) with actual state (connections) and converge."""
        desired: dict[uuid.UUID, tuple[str, str]] = {}  # stream_id -> (channel, access_token)

        async with async_session() as session:
            q = select(Stream).where(Stream.state == StreamState.LIVE)
            result = await session.execute(q)
            for stream in result.scalars():
                cred = await session.get(TwitchCredential, stream.credential_id)
                if cred is None or cred.channel_login is None:
                    continue
                if cred.oauth_access_ciphertext is None:
                    continue
                scopes = cred.oauth_scopes or []
                if not all(s in scopes for s in _REQUIRED_SCOPES):
                    continue
                token = credential_service.read_oauth_access(cred)
                if token is None:
                    continue
                desired[stream.id] = (cred.channel_login.lower(), token)

        # Drop connections that are no longer desired.
        for stream_id in list(self._connections.keys()):
            if stream_id not in desired:
                await self._drop(self._connections[stream_id])
                self._connections.pop(stream_id, None)

        # Start missing connections.
        for stream_id, (channel, token) in desired.items():
            if stream_id in self._connections:
                continue
            try:
                await self._spawn(stream_id, channel, token)
            except Exception:
                logger.exception("failed to spawn IRC client for stream %s", stream_id)

    async def _spawn(self, stream_id: uuid.UUID, channel: str, token: str) -> None:
        # Twitch IRC wants the user's own login as the NICK. We use the channel_login
        # of the credential (which, for a credential logged into its own channel, is
        # the same thing). Multi-channel ops would need the authenticated user's login.
        client = TwitchChatClient(token, nick=channel)
        await client.connect()
        await client.join(channel)

        pump = asyncio.create_task(
            self._pump(stream_id, client),
            name=f"orion.chat_pump:{stream_id.hex[:8]}",
        )
        self._connections[stream_id] = _Connection(
            stream_id=stream_id,
            channel=channel,
            client=client,
            pump=pump,
        )
        logger.info("chat supervisor: joined #%s for stream %s", channel, stream_id)

    async def _pump(self, stream_id: uuid.UUID, client: TwitchChatClient) -> None:
        try:
            async for msg in client.messages():
                await self._on_message(stream_id, msg)
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("chat pump for stream %s crashed", stream_id)

    async def _on_message(self, stream_id: uuid.UUID, msg: IrcMessage) -> None:
        sent_at = (
            datetime.fromtimestamp(msg.sent_at_ts / 1000, tz=UTC)
            if msg.sent_at_ts is not None
            else datetime.now(tz=UTC)
        )
        record = ChatMessage(
            stream_id=stream_id,
            channel=msg.channel,
            author_login=msg.author_login,
            author_id=msg.author_id,
            author_display=msg.author_display,
            content=msg.content,
            badges=msg.badges,
            emotes=msg.emotes,
            sent_at=sent_at,
        )
        async with async_session() as session:
            session.add(record)
            await session.commit()
            await session.refresh(record)

        await chat_bus.publish(
            msg.channel,
            {
                "id": record.id,
                "stream_id": str(stream_id),
                "channel": msg.channel,
                "author_login": msg.author_login,
                "author_display": msg.author_display,
                "author_id": msg.author_id,
                "content": msg.content,
                "badges": msg.badges,
                "emotes": msg.emotes,
                "sent_at": sent_at.isoformat(),
            },
        )

    async def _drop(self, conn: _Connection) -> None:
        logger.info("chat supervisor: dropping #%s for stream %s", conn.channel, conn.stream_id)
        conn.pump.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await conn.pump
        await conn.client.close()


supervisor = ChatSupervisor()
