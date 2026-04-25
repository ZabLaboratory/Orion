"""Stream lifecycle orchestrator.

The stream manager is the only place that mutates a ``Stream`` row's ``state`` column
and the only place that calls MediaMTX. Routes call into it; the routes never touch
MediaMTX directly.
"""

from __future__ import annotations

import logging
import secrets
import uuid
from datetime import UTC, datetime, timedelta

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from orion.config import settings
from orion.models.credential import TwitchCredential
from orion.models.stream import Stream, StreamState
from orion.services import encryption
from orion.services.mediamtx import (
    MediaMTXClient,
    StreamingParams,
    build_twitch_relay_config,
)

logger = logging.getLogger(__name__)


class StreamManagerError(RuntimeError):
    """Any unrecoverable failure while orchestrating a stream."""


def _path_name_for(stream_id: uuid.UUID) -> str:
    """Deterministic short path name used as the MediaMTX handle."""
    return f"orion_{stream_id.hex}"


def _build_whip_url(path_name: str, ingress_token: str) -> str:
    base = settings.mediamtx_public_whip_base.rstrip("/")
    return f"{base}/{path_name}/whip?token={ingress_token}"


def _streaming_params_from(stream: Stream) -> StreamingParams:
    return StreamingParams(
        width=stream.target_width,
        height=stream.target_height,
        fps=stream.target_fps,
        video_bitrate_kbps=stream.video_bitrate_kbps,
        audio_bitrate_kbps=stream.audio_bitrate_kbps,
        keyframe_interval_s=stream.keyframe_interval_s,
        encoder_preset=stream.encoder_preset,
    )


async def create_stream(
    db: AsyncSession,
    *,
    owner_id: uuid.UUID | None,
    overlay_id: uuid.UUID | None,
    credential_id: uuid.UUID,
    overlay_playlist: list[uuid.UUID] | None = None,
    target_width: int = 1920,
    target_height: int = 1080,
    target_fps: int = 30,
    video_bitrate_kbps: int = 6000,
    audio_bitrate_kbps: int = 160,
    keyframe_interval_s: int = 2,
    encoder_preset: str = "veryfast",
    record: bool = False,
    metadata: dict[str, object] | None = None,
) -> Stream:
    """Register a new stream in ``pending`` state.

    Does not talk to MediaMTX — call ``start_stream`` to allocate the path.
    ``overlay_id`` is a soft pointer into ZabCanvas (no FK is enforced); the
    stream renderer (Prism / ZabView) is the one that fetches and composes it.
    """
    credential = await db.get(TwitchCredential, credential_id)
    if credential is None:
        raise StreamManagerError(f"Credential {credential_id} not found")

    path_name = _path_name_for(uuid.uuid4())
    ingress_token = secrets.token_urlsafe(32)
    expires_at = datetime.now(tz=UTC) + timedelta(seconds=settings.ingress_token_ttl_seconds)

    # Soft-pointer ids on the wire are uuid.UUID; on disk they live in JSONB
    # as strings, so we serialise here once. The reverse path (read-side) is
    # handled by the Pydantic schema's UUID coercion.
    playlist_serialised = [str(o) for o in overlay_playlist or []]

    stream = Stream(
        owner_id=owner_id,
        overlay_id=overlay_id,
        overlay_playlist=playlist_serialised,
        credential_id=credential.id,
        state=StreamState.PENDING,
        mediamtx_path=path_name,
        whip_endpoint=_build_whip_url(path_name, ingress_token),
        ingress_token=ingress_token,
        ingress_token_expires_at=expires_at,
        target_width=target_width,
        target_height=target_height,
        target_fps=target_fps,
        video_bitrate_kbps=video_bitrate_kbps,
        audio_bitrate_kbps=audio_bitrate_kbps,
        keyframe_interval_s=keyframe_interval_s,
        encoder_preset=encoder_preset,
        record=record,
        metadata_=metadata or {},
    )
    db.add(stream)
    await db.flush()
    await db.refresh(stream)
    return stream


async def start_stream(
    db: AsyncSession,
    stream: Stream,
    mediamtx: MediaMTXClient,
) -> Stream:
    """Provision the MediaMTX path and flip the stream to ``preparing``.

    After this returns, the browser has everything it needs (``whip_endpoint``,
    ``ingress_token``) to POST its WebRTC offer. MediaMTX launches ffmpeg on
    first publish and the stream transitions to ``live`` on the next successful
    poll. ffmpeg consumes the streaming parameters stored on the stream row.
    """
    if stream.state not in (StreamState.PENDING, StreamState.ERROR, StreamState.ENDED):
        raise StreamManagerError(f"Stream {stream.id} cannot be started from state {stream.state.value}")

    credential = await db.get(TwitchCredential, stream.credential_id)
    if credential is None:
        raise StreamManagerError("Credential was deleted")

    stream_key = encryption.decrypt(credential.stream_key_ciphertext, credential.stream_key_nonce)
    config = build_twitch_relay_config(
        stream_key,
        stream.mediamtx_path,
        _streaming_params_from(stream),
        record=stream.record,
        stream_id=str(stream.id),
    )

    try:
        await mediamtx.replace_path(stream.mediamtx_path, config)
    except Exception as exc:
        stream.state = StreamState.ERROR
        stream.error_message = f"MediaMTX provisioning failed: {exc}"
        await db.flush()
        raise

    stream.state = StreamState.PREPARING
    stream.error_message = None
    new_token = secrets.token_urlsafe(32)
    stream.ingress_token = new_token
    stream.ingress_token_expires_at = datetime.now(tz=UTC) + timedelta(seconds=settings.ingress_token_ttl_seconds)
    stream.whip_endpoint = _build_whip_url(stream.mediamtx_path, new_token)
    await db.flush()
    await db.refresh(stream)
    return stream


async def mark_live(db: AsyncSession, stream: Stream) -> Stream:
    """Poller hook: set state to live when MediaMTX reports an active publisher."""
    if stream.state == StreamState.PREPARING:
        stream.state = StreamState.LIVE
        stream.started_at = datetime.now(tz=UTC)
        await db.flush()
    return stream


async def stop_stream(
    db: AsyncSession,
    stream: Stream,
    mediamtx: MediaMTXClient,
) -> Stream:
    """Tear down the MediaMTX path and end the stream."""
    if stream.state in (StreamState.ENDED, StreamState.STOPPING):
        return stream

    stream.state = StreamState.STOPPING
    await db.flush()

    try:
        await mediamtx.delete_path(stream.mediamtx_path)
    except Exception:
        logger.exception("MediaMTX delete_path failed for %s — forcing ENDED anyway", stream.id)

    stream.state = StreamState.ENDED
    stream.ended_at = datetime.now(tz=UTC)
    await db.flush()
    await db.refresh(stream)
    return stream


async def activate_overlay(
    db: AsyncSession,
    stream: Stream,
    overlay_id: uuid.UUID | None,
) -> Stream:
    """Switch the live overlay on this stream — the scene-switcher.

    Validates that ``overlay_id`` is in the stream's ``overlay_playlist``
    (or is ``None`` to clear the overlay) before persisting. The
    broadcaster picks up the change via the ``/streams/{id}/state``
    WebSocket and re-renders the canvas without tearing down its
    WHIP MediaStream — switching scenes mid-broadcast doesn't drop a
    frame on the wire.
    """
    if overlay_id is not None:
        playlist = stream.overlay_playlist or []
        if str(overlay_id) not in playlist:
            raise StreamManagerError(
                f"Overlay {overlay_id} is not in the stream's playlist; add it to "
                "``overlay_playlist`` before activating",
            )
    stream.overlay_id = overlay_id
    await db.flush()
    await db.refresh(stream)
    return stream


async def get_stream_by_ingress_token(db: AsyncSession, token: str) -> Stream | None:
    """Lookup used by the WHIP auth middleware to validate single-use tokens."""
    q = select(Stream).where(Stream.ingress_token == token)
    result = await db.execute(q)
    return result.scalar_one_or_none()
