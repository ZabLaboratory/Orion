"""Stream lifecycle routes."""

from __future__ import annotations

import uuid
from datetime import UTC, datetime

from fastapi import (
    APIRouter,
    Depends,
    HTTPException,
    Query,
    Response,
    WebSocket,
    WebSocketDisconnect,
    status,
)
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.models.stream import Stream, StreamState
from orion.routes._deps import authenticated_user, mediamtx_client
from orion.schemas.stream import (
    ActivateOverlayRequest,
    StreamCreate,
    StreamRead,
    StreamStartResponse,
    StreamSummary,
    StreamUpdate,
)
from orion.services import stream_manager
from orion.services.mediamtx import MediaMTXClient
from orion.services.stream_events import stream_event_bus

router = APIRouter(prefix="/streams", tags=["streams"])


@router.get("", response_model=list[StreamSummary])
async def list_streams(
    mine: bool = Query(default=False),
    state: StreamState | None = Query(default=None),
    db: AsyncSession = Depends(get_session),
    user_id: uuid.UUID | None = Depends(authenticated_user),
) -> list[Stream]:
    q = select(Stream).order_by(Stream.updated_at.desc())
    if mine and user_id is not None:
        q = q.where(Stream.owner_id == user_id)
    if state is not None:
        q = q.where(Stream.state == state)
    result = await db.execute(q)
    return list(result.scalars().all())


@router.post("", response_model=StreamRead, status_code=status.HTTP_201_CREATED)
async def create_stream(
    payload: StreamCreate,
    db: AsyncSession = Depends(get_session),
    user_id: uuid.UUID | None = Depends(authenticated_user),
) -> Stream:
    metadata: dict[str, object] = {
        **payload.metadata,
        "title": payload.title,
        "game_id": payload.game_id,
        "tags": payload.tags,
    }
    try:
        stream = await stream_manager.create_stream(
            db,
            owner_id=user_id,
            overlay_id=payload.overlay_id,
            overlay_playlist=payload.overlay_playlist,
            credential_id=payload.credential_id,
            target_width=payload.target_width,
            target_height=payload.target_height,
            target_fps=payload.target_fps,
            video_bitrate_kbps=payload.video_bitrate_kbps,
            audio_bitrate_kbps=payload.audio_bitrate_kbps,
            keyframe_interval_s=payload.keyframe_interval_s,
            encoder_preset=payload.encoder_preset,
            metadata=metadata,
        )
    except stream_manager.StreamManagerError as exc:
        raise HTTPException(status_code=status.HTTP_400_BAD_REQUEST, detail=str(exc)) from exc
    await db.commit()
    return stream


@router.get("/{stream_id}", response_model=StreamRead)
async def get_stream(
    stream_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
) -> Stream:
    stream = await db.get(Stream, stream_id)
    if stream is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Stream not found.")
    return stream


@router.put("/{stream_id}", response_model=StreamRead)
async def update_stream(
    stream_id: uuid.UUID,
    payload: StreamUpdate,
    db: AsyncSession = Depends(get_session),
) -> Stream:
    """Partial update — only fields present in the body are touched.

    Forbidden while the stream is currently LIVE/PREPARING (would silently
    drift from what the running ffmpeg session is actually doing). Stop the
    stream first if you need to swap parameters mid-session.
    """
    stream = await db.get(Stream, stream_id)
    if stream is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Stream not found.")
    if stream.state in (StreamState.PREPARING, StreamState.LIVE):
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail="Stop the stream before editing its parameters.",
        )

    data = payload.model_dump(exclude_unset=True)
    if "metadata" in data:
        stream.metadata_ = data.pop("metadata") or {}
    if "overlay_playlist" in data:
        # JSONB stores str(uuid) — coerce here so downstream reads stay
        # consistent whether the row was just inserted or pulled from disk.
        playlist = data.pop("overlay_playlist") or []
        stream.overlay_playlist = [str(o) for o in playlist]
    for key, value in data.items():
        setattr(stream, key, value)
    await db.flush()
    await db.refresh(stream)
    await db.commit()
    return stream


@router.post("/{stream_id}/active-overlay", response_model=StreamRead)
async def activate_overlay_endpoint(
    stream_id: uuid.UUID,
    payload: ActivateOverlayRequest,
    db: AsyncSession = Depends(get_session),
) -> Stream:
    """Switch the stream's currently active overlay — the scene-switcher.

    Unlike ``PUT /streams/{id}`` which forbids edits while the stream is
    LIVE, this endpoint is **explicitly designed to fire mid-broadcast**.
    The new overlay must already exist in ``stream.overlay_playlist`` —
    operators pre-declare the set of switchable scenes when authoring
    the stream so a stale macro can't blank the broadcast.
    """
    stream = await db.get(Stream, stream_id)
    if stream is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Stream not found.")
    try:
        stream = await stream_manager.activate_overlay(db, stream, payload.overlay_id)
    except stream_manager.StreamManagerError as exc:
        raise HTTPException(status_code=status.HTTP_400_BAD_REQUEST, detail=str(exc)) from exc
    await db.commit()
    await stream_event_bus.publish(
        str(stream.id),
        {
            "event": "active_overlay_changed",
            "stream_id": str(stream.id),
            "overlay_id": str(stream.overlay_id) if stream.overlay_id else None,
            "at": datetime.now(tz=UTC).isoformat(),
        },
    )
    return stream


@router.post("/{stream_id}/start", response_model=StreamStartResponse)
async def start_stream(
    stream_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
    mtx: MediaMTXClient = Depends(mediamtx_client),
) -> StreamStartResponse:
    stream = await db.get(Stream, stream_id)
    if stream is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Stream not found.")
    try:
        stream = await stream_manager.start_stream(db, stream, mtx)
    except stream_manager.StreamManagerError as exc:
        raise HTTPException(status_code=status.HTTP_400_BAD_REQUEST, detail=str(exc)) from exc
    await db.commit()
    await _publish_state(stream)
    return StreamStartResponse(
        stream=StreamRead.model_validate(stream),
        whip_url=stream.whip_endpoint,
        ice_servers=[],
    )


@router.post("/{stream_id}/stop", response_model=StreamRead)
async def stop_stream(
    stream_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
    mtx: MediaMTXClient = Depends(mediamtx_client),
) -> Stream:
    stream = await db.get(Stream, stream_id)
    if stream is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Stream not found.")
    stream = await stream_manager.stop_stream(db, stream, mtx)
    await db.commit()
    await _publish_state(stream)
    return stream


async def _publish_state(stream: Stream) -> None:
    """Publish a ``state_changed`` event for the stream's current state.

    Called from every route that flips the lifecycle (start/stop, and
    indirectly from any future action that touches ``state``). Subscribers
    on ``WS /streams/{id}/state`` see lifecycle and overlay-switch events
    on the same channel — one socket, both feeds.
    """
    await stream_event_bus.publish(
        str(stream.id),
        {
            "event": "state_changed",
            "stream_id": str(stream.id),
            "state": stream.state.value,
            "at": datetime.now(tz=UTC).isoformat(),
        },
    )


@router.websocket("/{stream_id}/state")
async def stream_state_socket(websocket: WebSocket, stream_id: uuid.UUID) -> None:
    """Live stream of state events for one stream.

    Forwards everything ``stream_event_bus`` publishes for this stream id
    as JSON frames — ``active_overlay_changed`` (scene-switcher),
    ``state_changed`` (lifecycle), and any future event types. Subscribers
    that don't recognise an ``event`` string should ignore it: this socket
    is additive on purpose.

    Unlike ``WS /chat/live/{channel}``, the queue is small (128) — state
    events are infrequent and a backed-up subscriber dropping a few
    frames is acceptable; they can re-fetch ``GET /streams/{id}`` to
    re-sync.
    """
    await websocket.accept()
    queue = stream_event_bus.subscribe(str(stream_id))
    try:
        while True:
            event = await queue.get()
            await websocket.send_json(event)
    except WebSocketDisconnect:
        return
    finally:
        stream_event_bus.unsubscribe(str(stream_id), queue)


@router.delete("/{stream_id}", status_code=status.HTTP_204_NO_CONTENT)
async def delete_stream(
    stream_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
    mtx: MediaMTXClient = Depends(mediamtx_client),
) -> Response:
    stream = await db.get(Stream, stream_id)
    if stream is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Stream not found.")
    if stream.state in (StreamState.PREPARING, StreamState.LIVE):
        await stream_manager.stop_stream(db, stream, mtx)
    await db.delete(stream)
    await db.commit()
    return Response(status_code=status.HTTP_204_NO_CONTENT)
