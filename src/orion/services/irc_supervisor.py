"""Subscriber-driven IRC supervisor.

Replaces the streams-driven supervisor that was retired at the v0.4.0
pivot. The model is now :

* a WS client subscribes to ``/api/v1/chat/live/{channel}`` →
  :func:`acquire` is called ; on the first subscriber for a channel
  the supervisor finds a credential for it, decrypts the OAuth token,
  opens an IRC client, joins, and starts a pump task that drops
  decoded messages into ``chat_bus`` (and persists each message to
  ``chat_messages``).
* the WS client disconnects → :func:`release` decrements the ref
  count ; on the last subscriber a close task is scheduled to run
  after :data:`GRACE_SECONDS` so a quick reconnect doesn't trigger
  a churn cycle.
* an :func:`acquire` during the grace window cancels the close.

The supervisor is best-effort : if no credential with chat scopes
exists for the requested channel, or if the IRC handshake fails, the
WS subscriber simply sees an empty stream — never an error.

Concurrency : per-channel setup lock so two simultaneous acquires
don't race the IRC handshake. Pumps are independent tasks that the
supervisor cancels on shutdown.

Stack swappable for tests : :data:`_factory` produces the IRC client
and :data:`_credential_lookup` finds a credential ; both can be
monkeypatched in unit tests so we never reach the real Twitch IRC.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import TYPE_CHECKING

from sqlalchemy import select

from orion.database import async_session
from orion.models.chat import ChatMessage as ChatMessageRow
from orion.models.credential import TwitchCredential
from orion.services import credential_service
from orion.services.chat_service import chat_bus
from orion.services.twitch_chat import ChatMessage as IrcMessage
from orion.services.twitch_chat import TwitchChatClient

if TYPE_CHECKING:
    from collections.abc import Awaitable as _AwaitableT  # noqa: F401

logger = logging.getLogger(__name__)

GRACE_SECONDS: float = 60.0
_REQUIRED_SCOPES: tuple[str, ...] = ("chat:read",)


@dataclass
class _ChannelState:
    ref_count: int = 0
    client: TwitchChatClient | None = None
    pump: asyncio.Task[None] | None = None
    close_task: asyncio.Task[None] | None = None
    setup_lock: asyncio.Lock = field(default_factory=asyncio.Lock)


# Type aliases for swappable hooks ; tests replace these to avoid touching
# real Twitch infrastructure.
ClientFactory = Callable[[str, str], TwitchChatClient]
CredentialLookup = Callable[[str], "Awaitable[tuple[TwitchCredential, str] | None]"]


def _default_factory(token: str, nick: str) -> TwitchChatClient:
    return TwitchChatClient(token, nick=nick)


async def _default_credential_lookup(
    channel_login: str,
) -> tuple[TwitchCredential, str] | None:
    """Find a credential that can join ``channel_login`` and return the
    pair ``(credential, decrypted_access_token)``.

    Returns ``None`` if no credential matches or the OAuth scopes don't
    include ``chat:read``.
    """
    async with async_session() as session:
        stmt = (
            select(TwitchCredential)
            .where(TwitchCredential.channel_login.is_not(None))
            .order_by(TwitchCredential.updated_at.desc())
        )
        result = await session.execute(stmt)
        for cred in result.scalars():
            if cred.channel_login is None:
                continue
            if cred.channel_login.lower() != channel_login.lower():
                continue
            if cred.oauth_access_ciphertext is None:
                continue
            scopes = cred.oauth_scopes or []
            if not all(s in scopes for s in _REQUIRED_SCOPES):
                continue
            token = credential_service.read_oauth_access(cred)
            if token is None:
                continue
            return cred, token
    return None


class IrcSupervisor:
    """Reference-counted IRC fleet driven by WS subscriptions."""

    def __init__(self) -> None:
        self._channels: dict[str, _ChannelState] = {}
        self._global_lock = asyncio.Lock()
        self._factory: ClientFactory = _default_factory
        self._lookup: CredentialLookup = _default_credential_lookup

    # ── Test seams ────────────────────────────────────────────────────

    def set_client_factory(self, factory: ClientFactory) -> None:
        self._factory = factory

    def set_credential_lookup(self, lookup: CredentialLookup) -> None:
        self._lookup = lookup

    # ── Public API ────────────────────────────────────────────────────

    async def acquire(self, channel_login: str) -> None:
        """Increment the ref count for ``channel_login`` and ensure an
        IRC client is running. Best-effort : missing credential or IRC
        failure is logged, never raised."""
        key = channel_login.lower()
        async with self._global_lock:
            state = self._channels.setdefault(key, _ChannelState())
            state.ref_count += 1
            if state.close_task is not None:
                state.close_task.cancel()
                state.close_task = None

        async with state.setup_lock:
            if state.client is not None:
                return
            try:
                cred_pair = await self._lookup(key)
                if cred_pair is None:
                    logger.info(
                        "irc supervisor: no credential with chat:read for #%s, "
                        "subscriber gets an idle stream",
                        key,
                    )
                    return
                cred, token = cred_pair
                client = self._factory(token, cred.channel_login or key)
                await client.connect()
                await client.join(key)
                pump = asyncio.create_task(
                    self._pump(key, client),
                    name=f"orion.irc_pump:{key}",
                )
                state.client = client
                state.pump = pump
                logger.info("irc supervisor: joined #%s (ref_count=%d)", key, state.ref_count)
            except Exception:
                logger.exception("irc supervisor: failed to open IRC for #%s", key)

    async def release(self, channel_login: str) -> None:
        """Decrement the ref count. Schedules a close after the grace
        window when the last subscriber leaves."""
        key = channel_login.lower()
        async with self._global_lock:
            state = self._channels.get(key)
            if state is None:
                return
            state.ref_count = max(0, state.ref_count - 1)
            if state.ref_count == 0 and state.client is not None and state.close_task is None:
                state.close_task = asyncio.create_task(
                    self._delayed_close(key),
                    name=f"orion.irc_grace:{key}",
                )

    async def shutdown(self) -> None:
        """Tear down every open IRC client. Called from the FastAPI
        lifespan on app shutdown."""
        for key, state in list(self._channels.items()):
            if state.close_task is not None:
                state.close_task.cancel()
            await self._teardown(key, state)

    # ── Internals ─────────────────────────────────────────────────────

    async def _delayed_close(self, key: str) -> None:
        try:
            await asyncio.sleep(GRACE_SECONDS)
        except asyncio.CancelledError:
            return
        async with self._global_lock:
            state = self._channels.get(key)
            if state is None or state.ref_count > 0:
                return
        await self._teardown(key, state)

    async def _teardown(self, key: str, state: _ChannelState) -> None:
        async with state.setup_lock:
            if state.pump is not None:
                state.pump.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await state.pump
                state.pump = None
            if state.client is not None:
                await state.client.close()
                state.client = None
            state.close_task = None
            logger.info("irc supervisor: dropped #%s", key)

    async def _pump(self, key: str, client: TwitchChatClient) -> None:
        try:
            async for msg in client.messages():
                await self._on_message(key, msg)
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("irc pump for #%s crashed", key)

    async def _on_message(self, key: str, msg: IrcMessage) -> None:
        sent_at = (
            datetime.fromtimestamp(msg.sent_at_ts / 1000, tz=UTC)
            if msg.sent_at_ts is not None
            else datetime.now(tz=UTC)
        )
        record = ChatMessageRow(
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


supervisor = IrcSupervisor()
