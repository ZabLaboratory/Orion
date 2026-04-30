"""In-process pub/sub for EventSub notifications.

Mirrors :mod:`orion.services.chat_service` — per-channel queues that
the WS fan-out endpoint drains. Channel here is keyed by
``broadcaster_user_id`` (string, Twitch's internal numeric id) so
blueprint consumers don't need to know which credential is the
owner.

Replace with Redis pub/sub if Orion ever scales beyond one replica.
"""

from __future__ import annotations

import asyncio
from collections import defaultdict


class EventSubBus:
    """Per-broadcaster queues. Drops oldest on overflow."""

    def __init__(self) -> None:
        self._subscribers: dict[str, list[asyncio.Queue[dict[str, object]]]] = defaultdict(list)

    def subscribe(self, broadcaster_id: str) -> asyncio.Queue[dict[str, object]]:
        q: asyncio.Queue[dict[str, object]] = asyncio.Queue(maxsize=1024)
        self._subscribers[broadcaster_id].append(q)
        return q

    def unsubscribe(self, broadcaster_id: str, q: asyncio.Queue[dict[str, object]]) -> None:
        lst = self._subscribers.get(broadcaster_id)
        if lst and q in lst:
            lst.remove(q)

    async def publish(self, broadcaster_id: str, event: dict[str, object]) -> None:
        for q in list(self._subscribers.get(broadcaster_id, [])):
            if q.full():
                try:
                    q.get_nowait()
                except asyncio.QueueEmpty:
                    pass
            await q.put(event)


eventsub_bus = EventSubBus()
