"""In-process pub/sub for stream state changes.

Mirrors ``chat_service.ChatEventBus`` but keyed by stream id (UUID
stringified). The scene-switcher endpoint publishes
``active_overlay_changed`` here; the ``/streams/{id}/state`` WebSocket
forwards events to subscribed clients (broadcaster, mobile companion,
Stream Deck plugin via Companion module). Same lossy semantics — state
events should never block a database commit.

Events published today:

- ``active_overlay_changed`` — operator switched the live overlay.
  Payload: ``{event, stream_id, overlay_id, at}``.
- ``state_changed`` — stream lifecycle transition (pending → preparing
  → live → stopping → ended | error). Payload: ``{event, stream_id,
  state, at}``.

New event types are additive: subscribers ignore unknown ``event``
strings, so adding ``audio_mute_changed`` (Phase 5) or
``viewer_count_updated`` doesn't break existing clients.

Replace with Redis pub/sub if Orion ever scales beyond one replica.
"""

from __future__ import annotations

import asyncio
from collections import defaultdict
from typing import Any


class StreamEventBus:
    """Per-stream queues. Drops oldest on overflow."""

    def __init__(self) -> None:
        self._subscribers: dict[str, list[asyncio.Queue[dict[str, Any]]]] = defaultdict(list)

    def subscribe(self, stream_id: str) -> asyncio.Queue[dict[str, Any]]:
        q: asyncio.Queue[dict[str, Any]] = asyncio.Queue(maxsize=128)
        self._subscribers[stream_id].append(q)
        return q

    def unsubscribe(self, stream_id: str, q: asyncio.Queue[dict[str, Any]]) -> None:
        lst = self._subscribers.get(stream_id)
        if lst and q in lst:
            lst.remove(q)
            if not lst:
                self._subscribers.pop(stream_id, None)

    async def publish(self, stream_id: str, event: dict[str, Any]) -> None:
        for q in list(self._subscribers.get(stream_id, [])):
            if q.full():
                try:
                    q.get_nowait()
                except asyncio.QueueEmpty:
                    pass
            await q.put(event)


stream_event_bus = StreamEventBus()
