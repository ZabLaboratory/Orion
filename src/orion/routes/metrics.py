"""Stream metrics — historical pull + live WebSocket."""

from __future__ import annotations

import asyncio
import uuid

from fastapi import APIRouter, Depends, WebSocket, WebSocketDisconnect
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import async_session, get_session
from orion.models.metric import StreamMetric
from orion.models.stream import Stream, StreamState
from orion.routes._deps import mediamtx_client
from orion.schemas.metric import StreamMetricRead
from orion.services.mediamtx import MediaMTXClient

router = APIRouter(prefix="/metrics", tags=["metrics"])


@router.get("/streams/{stream_id}", response_model=list[StreamMetricRead])
async def stream_metrics(
    stream_id: uuid.UUID,
    limit: int = 120,
    db: AsyncSession = Depends(get_session),
) -> list[StreamMetric]:
    q = (
        select(StreamMetric)
        .where(StreamMetric.stream_id == stream_id)
        .order_by(StreamMetric.ts.desc())
        .limit(max(1, min(limit, 1000)))
    )
    result = await db.execute(q)
    return list(result.scalars().all())


@router.websocket("/streams/{stream_id}/live")
async def stream_metrics_live(
    websocket: WebSocket,
    stream_id: uuid.UUID,
    mtx: MediaMTXClient = Depends(mediamtx_client),
) -> None:
    """Polls MediaMTX every 2s while the stream is LIVE/PREPARING and pushes state.

    Single-source-of-truth on the DB side. We do not persist the snapshots the WS
    reads — the stream_manager persists a coarser time series separately.
    """
    await websocket.accept()
    try:
        while True:
            async with async_session() as session:
                stream = await session.get(Stream, stream_id)
            if stream is None:
                await websocket.send_json({"error": "stream not found"})
                return

            info = await mtx.get_path(stream.mediamtx_path)
            payload = {
                "state": stream.state.value,
                "started_at": stream.started_at.isoformat() if stream.started_at else None,
                "ended_at": stream.ended_at.isoformat() if stream.ended_at else None,
                "mediamtx": info or {},
            }
            await websocket.send_json(payload)
            if stream.state in (StreamState.ENDED, StreamState.ERROR):
                return
            await asyncio.sleep(2.0)
    except WebSocketDisconnect:
        return
