"""Scene-switcher tests — playlist validation + state event publishing.

The model layer uses PostgreSQL types (ARRAY, JSONB) that don't
compile on SQLite, so DB-touching tests would need a real Postgres.
Here we exercise the pure-Python pieces:

- ``stream_manager.activate_overlay`` validation (operates on a Stream
  object handed to it — no DB roundtrip required for the validation
  branch).
- ``StreamEventBus`` pub/sub semantics.

Full HTTP-level coverage of the new endpoint is delegated to the
``e2e`` smoke tests that run against a real Postgres in CI."""

import uuid
from unittest.mock import AsyncMock

import pytest

from orion.models.stream import Stream, StreamState
from orion.services import stream_manager
from orion.services.stream_events import StreamEventBus


def _stub_stream(*, playlist: list[uuid.UUID]) -> Stream:
    """A bare Stream object — no DB row. Enough for the validation
    branch of ``activate_overlay`` since no ORM round-trip is
    required when the call is rejected."""
    s = Stream()
    s.id = uuid.uuid4()
    s.owner_id = None
    s.overlay_id = None
    s.overlay_playlist = [str(o) for o in playlist]
    s.credential_id = uuid.uuid4()
    s.state = StreamState.PENDING
    s.mediamtx_path = "test"
    s.whip_endpoint = "http://x"
    s.ingress_token = "tk"
    return s


@pytest.mark.asyncio
async def test_activate_overlay_rejects_ids_outside_playlist() -> None:
    """A scene switch refuses ids outside ``overlay_playlist`` —
    defends macro/mobile clients from a stale id blanking the
    broadcast."""
    stream = _stub_stream(playlist=[uuid.uuid4()])
    rogue = uuid.uuid4()

    db = AsyncMock()
    with pytest.raises(stream_manager.StreamManagerError) as exc:
        await stream_manager.activate_overlay(db, stream, rogue)
    assert "playlist" in str(exc.value)
    # Nothing flushed — rejected before DB touched.
    db.flush.assert_not_called()


@pytest.mark.asyncio
async def test_activate_overlay_clear_to_none_always_allowed() -> None:
    """``overlay_id=None`` (raw camera fallback) is always permitted,
    regardless of the playlist contents."""
    stream = _stub_stream(playlist=[])
    stream.overlay_id = uuid.uuid4()  # something to clear
    db = AsyncMock()

    result = await stream_manager.activate_overlay(db, stream, None)
    assert result.overlay_id is None
    db.flush.assert_called_once()


@pytest.mark.asyncio
async def test_event_bus_subscribers_receive_published_events() -> None:
    """The pub/sub primitive used by ``WS /streams/{id}/state``: a
    subscriber receives every event published for its stream id and
    only those."""
    bus = StreamEventBus()
    sid = "stream-A"
    other = "stream-B"

    queue = bus.subscribe(sid)

    await bus.publish(sid, {"event": "active_overlay_changed", "stream_id": sid})
    await bus.publish(other, {"event": "active_overlay_changed", "stream_id": other})
    await bus.publish(sid, {"event": "state_changed", "stream_id": sid, "state": "live"})

    # Drain — only events for `sid` should arrive.
    received = [queue.get_nowait() for _ in range(2)]
    events = [e["event"] for e in received]
    assert events == ["active_overlay_changed", "state_changed"]
    assert queue.empty()  # the `other` event was filtered out at publish time

    bus.unsubscribe(sid, queue)


@pytest.mark.asyncio
async def test_event_bus_drops_oldest_on_overflow() -> None:
    """The bus is lossy by design — a slow subscriber can't pin RAM. On
    overflow the oldest event is dropped so newer state stays visible."""
    bus = StreamEventBus()
    sid = "stream-overflow"
    queue = bus.subscribe(sid)

    # Saturate the queue beyond its 128 capacity.
    for i in range(135):
        await bus.publish(sid, {"event": "state_changed", "seq": i})

    drained = []
    while not queue.empty():
        drained.append(queue.get_nowait()["seq"])
    # Oldest entries dropped — survivors are the most recent 128.
    assert len(drained) == 128
    assert drained[0] >= 7  # first 7 dropped (135 - 128)
    assert drained[-1] == 134

    bus.unsubscribe(sid, queue)
