"""StreamDestination CRUD — additional RTMP outputs per stream.

Mounted under ``/streams/{stream_id}/destinations`` so the URL
hierarchy mirrors the data model. The legacy ``streams.credential_id``
Twitch path stays untouched.
"""

from __future__ import annotations

import uuid

from fastapi import APIRouter, Depends, HTTPException, status
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.models import Stream, StreamDestination
from orion.schemas.destination import (
    StreamDestinationCreate,
    StreamDestinationRead,
    StreamDestinationUpdate,
)
from orion.services import encryption

router = APIRouter(prefix="/streams/{stream_id}/destinations", tags=["destinations"])


async def _ensure_stream(stream_id: uuid.UUID, db: AsyncSession) -> Stream:
    stream = await db.get(Stream, stream_id)
    if stream is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Stream not found.")
    return stream


@router.get("", response_model=list[StreamDestinationRead])
async def list_destinations(
    stream_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
) -> list[StreamDestination]:
    await _ensure_stream(stream_id, db)
    rows = await db.execute(
        select(StreamDestination)
        .where(StreamDestination.stream_id == stream_id)
        .order_by(StreamDestination.ordering, StreamDestination.created_at),
    )
    return list(rows.scalars().all())


@router.post("", response_model=StreamDestinationRead, status_code=status.HTTP_201_CREATED)
async def create_destination(
    stream_id: uuid.UUID,
    payload: StreamDestinationCreate,
    db: AsyncSession = Depends(get_session),
) -> StreamDestination:
    await _ensure_stream(stream_id, db)
    url_ct, url_nonce = encryption.encrypt(payload.rtmp_url)
    key_ct, key_nonce = encryption.encrypt(payload.stream_key)
    dest = StreamDestination(
        stream_id=stream_id,
        kind=payload.kind,
        display_name=payload.display_name,
        rtmp_url_ciphertext=url_ct,
        rtmp_url_nonce=url_nonce,
        stream_key_ciphertext=key_ct,
        stream_key_nonce=key_nonce,
        enabled=payload.enabled,
        ordering=payload.ordering,
    )
    db.add(dest)
    await db.commit()
    await db.refresh(dest)
    return dest


@router.get("/{destination_id}", response_model=StreamDestinationRead)
async def get_destination(
    stream_id: uuid.UUID,
    destination_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
) -> StreamDestination:
    await _ensure_stream(stream_id, db)
    dest = await db.get(StreamDestination, destination_id)
    if dest is None or dest.stream_id != stream_id:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Destination not found.")
    return dest


@router.put("/{destination_id}", response_model=StreamDestinationRead)
async def update_destination(
    stream_id: uuid.UUID,
    destination_id: uuid.UUID,
    payload: StreamDestinationUpdate,
    db: AsyncSession = Depends(get_session),
) -> StreamDestination:
    await _ensure_stream(stream_id, db)
    dest = await db.get(StreamDestination, destination_id)
    if dest is None or dest.stream_id != stream_id:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Destination not found.")
    data = payload.model_dump(exclude_unset=True)
    if "rtmp_url" in data:
        ct, nonce = encryption.encrypt(data.pop("rtmp_url"))
        dest.rtmp_url_ciphertext = ct
        dest.rtmp_url_nonce = nonce
    if "stream_key" in data:
        ct, nonce = encryption.encrypt(data.pop("stream_key"))
        dest.stream_key_ciphertext = ct
        dest.stream_key_nonce = nonce
    for key, value in data.items():
        setattr(dest, key, value)
    await db.commit()
    await db.refresh(dest)
    return dest


@router.delete("/{destination_id}", status_code=status.HTTP_204_NO_CONTENT)
async def delete_destination(
    stream_id: uuid.UUID,
    destination_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
) -> None:
    await _ensure_stream(stream_id, db)
    dest = await db.get(StreamDestination, destination_id)
    if dest is None or dest.stream_id != stream_id:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Destination not found.")
    await db.delete(dest)
    await db.commit()
