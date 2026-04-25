"""Stream lifecycle routes."""

from __future__ import annotations

import uuid

from fastapi import APIRouter, Depends, HTTPException, Query, Response, status
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.models.stream import Stream, StreamState
from orion.routes._deps import authenticated_user, mediamtx_client
from orion.schemas.stream import (
    StreamCreate,
    StreamRead,
    StreamStartResponse,
    StreamSummary,
    StreamUpdate,
)
from orion.services import stream_manager
from orion.services.mediamtx import MediaMTXClient

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
    for key, value in data.items():
        setattr(stream, key, value)
    await db.flush()
    await db.refresh(stream)
    await db.commit()
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
    return stream


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
