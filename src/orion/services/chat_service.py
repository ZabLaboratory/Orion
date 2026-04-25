"""In-process pub/sub used to fan chat messages out to connected WebSockets.

Replace with Redis pub/sub if Orion ever scales beyond one replica.
"""

from __future__ import annotations

import asyncio
from collections import defaultdict


class ChatEventBus:
    """Per-channel queues. Drops oldest on overflow — chat is lossy by design."""

    def __init__(self) -> None:
        self._subscribers: dict[str, list[asyncio.Queue[dict[str, object]]]] = defaultdict(list)

    def subscribe(self, channel: str) -> asyncio.Queue[dict[str, object]]:
        q: asyncio.Queue[dict[str, object]] = asyncio.Queue(maxsize=1024)
        self._subscribers[channel.lower()].append(q)
        return q

    def unsubscribe(self, channel: str, q: asyncio.Queue[dict[str, object]]) -> None:
        lst = self._subscribers.get(channel.lower())
        if lst and q in lst:
            lst.remove(q)

    async def publish(self, channel: str, event: dict[str, object]) -> None:
        for q in list(self._subscribers.get(channel.lower(), [])):
            if q.full():
                try:
                    q.get_nowait()
                except asyncio.QueueEmpty:
                    pass
            await q.put(event)


chat_bus = ChatEventBus()
