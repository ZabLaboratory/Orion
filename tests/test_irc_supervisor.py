"""IRC supervisor — subscriber-driven IRC fleet.

Tests use a stub IRC client + a stub credential lookup so they never
touch real Twitch infrastructure. The supervisor is the global
singleton ; each test resets its state between runs.
"""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator, Awaitable
from dataclasses import dataclass
from typing import Any

import pytest

from orion.services import irc_supervisor as supervisor_module
from orion.services.irc_supervisor import IrcSupervisor
from orion.services.twitch_chat import ChatMessage as IrcMessage


@dataclass
class FakeCredential:
    """Drop-in for :class:`TwitchCredential` — only the fields the
    supervisor reads."""

    channel_login: str


class FakeChatClient:
    """Drop-in for :class:`TwitchChatClient` with no network at all.

    Calls are recorded so tests can assert lifecycle order and counts.
    Messages can be pushed in via :meth:`push_message` which feeds the
    pump.
    """

    instances: list[FakeChatClient] = []

    def __init__(self, token: str, nick: str) -> None:
        self.token = token
        self.nick = nick
        self.connected = False
        self.closed = False
        self.joined: set[str] = set()
        self._inbox: asyncio.Queue[IrcMessage | None] = asyncio.Queue()
        FakeChatClient.instances.append(self)

    async def connect(self) -> None:
        self.connected = True

    async def join(self, channel: str) -> None:
        self.joined.add(channel.lower().lstrip("#"))

    async def messages(self) -> AsyncIterator[IrcMessage]:
        while True:
            item = await self._inbox.get()
            if item is None:
                return
            yield item

    async def close(self) -> None:
        self.closed = True
        await self._inbox.put(None)

    async def push_message(self, msg: IrcMessage) -> None:
        await self._inbox.put(msg)


@pytest.fixture(autouse=True)
def _reset_fake_clients() -> None:
    FakeChatClient.instances.clear()


@pytest.fixture
def fast_supervisor(monkeypatch: pytest.MonkeyPatch) -> IrcSupervisor:
    """Fresh supervisor instance with stubs and a 0s grace window so
    teardown happens immediately."""
    monkeypatch.setattr(supervisor_module, "GRACE_SECONDS", 0.0)
    sup = IrcSupervisor()
    sup.set_client_factory(FakeChatClient)  # type: ignore[arg-type]

    async def lookup(channel: str) -> tuple[Any, str] | None:
        return FakeCredential(channel_login=channel), "test-token"

    sup.set_credential_lookup(lookup)  # type: ignore[arg-type]
    return sup


@pytest.fixture
def supervisor_no_credential(monkeypatch: pytest.MonkeyPatch) -> IrcSupervisor:
    """Supervisor whose credential lookup always returns None."""
    monkeypatch.setattr(supervisor_module, "GRACE_SECONDS", 0.0)
    sup = IrcSupervisor()
    sup.set_client_factory(FakeChatClient)  # type: ignore[arg-type]

    async def lookup(channel: str) -> Awaitable[None] | None:
        return None

    sup.set_credential_lookup(lookup)  # type: ignore[arg-type]
    return sup


# ── acquire / release lifecycle ────────────────────────────────────────────


async def test_first_acquire_opens_client(fast_supervisor: IrcSupervisor) -> None:
    await fast_supervisor.acquire("starbucks")
    assert len(FakeChatClient.instances) == 1
    client = FakeChatClient.instances[0]
    assert client.connected is True
    assert "starbucks" in client.joined


async def test_second_acquire_reuses_client(fast_supervisor: IrcSupervisor) -> None:
    await fast_supervisor.acquire("starbucks")
    await fast_supervisor.acquire("starbucks")
    assert len(FakeChatClient.instances) == 1


async def test_release_below_zero_keeps_client(fast_supervisor: IrcSupervisor) -> None:
    await fast_supervisor.acquire("starbucks")
    await fast_supervisor.acquire("starbucks")
    await fast_supervisor.release("starbucks")
    # Still one subscriber → client stays.
    client = FakeChatClient.instances[0]
    assert client.closed is False


async def test_last_release_schedules_close(fast_supervisor: IrcSupervisor) -> None:
    await fast_supervisor.acquire("starbucks")
    await fast_supervisor.release("starbucks")
    # GRACE_SECONDS=0 — wait one tick for the grace task to run.
    for _ in range(20):
        await asyncio.sleep(0)
        if FakeChatClient.instances[0].closed:
            break
    assert FakeChatClient.instances[0].closed is True


async def test_acquire_during_grace_cancels_close(fast_supervisor: IrcSupervisor) -> None:
    await fast_supervisor.acquire("starbucks")
    await fast_supervisor.release("starbucks")  # schedules close
    await fast_supervisor.acquire("starbucks")  # cancels it
    # Pump the loop briefly — close task must NOT have fired.
    for _ in range(10):
        await asyncio.sleep(0)
    assert FakeChatClient.instances[0].closed is False
    assert len(FakeChatClient.instances) == 1


async def test_acquire_distinct_channels_opens_distinct_clients(
    fast_supervisor: IrcSupervisor,
) -> None:
    await fast_supervisor.acquire("starbucks")
    await fast_supervisor.acquire("trihex")
    assert len(FakeChatClient.instances) == 2
    joined = {next(iter(c.joined)) for c in FakeChatClient.instances}
    assert joined == {"starbucks", "trihex"}


async def test_acquire_with_no_credential_silently_idle(
    supervisor_no_credential: IrcSupervisor,
) -> None:
    """No credential → no IRC client. Subscriber sees an idle stream."""
    await supervisor_no_credential.acquire("ghost")
    assert FakeChatClient.instances == []


async def test_shutdown_tears_down_all_clients(fast_supervisor: IrcSupervisor) -> None:
    await fast_supervisor.acquire("starbucks")
    await fast_supervisor.acquire("trihex")
    await fast_supervisor.shutdown()
    for client in FakeChatClient.instances:
        assert client.closed is True


# ── concurrent acquires don't double-open ──────────────────────────────────


async def test_concurrent_acquires_open_one_client(fast_supervisor: IrcSupervisor) -> None:
    await asyncio.gather(
        fast_supervisor.acquire("starbucks"),
        fast_supervisor.acquire("starbucks"),
        fast_supervisor.acquire("starbucks"),
    )
    assert len(FakeChatClient.instances) == 1
