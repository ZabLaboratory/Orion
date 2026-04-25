"""Stream model — lifecycle of a single broadcast session."""

from __future__ import annotations

import enum
import uuid
from datetime import datetime
from typing import Any

from sqlalchemy import Boolean, DateTime, Enum, ForeignKey, Integer, String
from sqlalchemy.dialects.postgresql import JSONB, UUID
from sqlalchemy.orm import Mapped, mapped_column

from orion.models.base import Base, created_at_col, updated_at_col, uuid_pk


class StreamState(enum.StrEnum):
    """Stream lifecycle states.

    pending → preparing → live → stopping → ended
                       ↘ error
    """

    PENDING = "pending"
    PREPARING = "preparing"
    LIVE = "live"
    STOPPING = "stopping"
    ENDED = "ended"
    ERROR = "error"


class Stream(Base):
    """One broadcast session.

    Orion is the streaming control plane — it does NOT author scenes. The visual
    composition lives in ZabCanvas (`overlays`); blueprint-backed components are
    hydrated by Blue at render time. ``overlay_id`` is a soft pointer into
    ZabCanvas (no cross-service FK on purpose — services own their own
    integrity).

    The streamer's client (Prism today, ZabView fallback) loads the overlay,
    asks ZabCanvas to resolve blueprints, composes the result onto a canvas,
    and pushes the canvas as a WebRTC track to the WHIP URL Orion mints. ffmpeg
    inside MediaMTX transcodes that WebRTC stream to H264/AAC and pushes RTMP
    to Twitch using the decrypted stream key — with the bitrate/resolution/fps
    knobs configured here.
    """

    __tablename__ = "streams"

    id: Mapped[uuid.UUID] = uuid_pk()
    owner_id: Mapped[uuid.UUID | None] = mapped_column(UUID(as_uuid=True), nullable=True, index=True)

    # Soft pointer into ZabCanvas — the *currently active* overlay. No FK —
    # Orion mustn't crash if ZabCanvas is offline or if the overlay is later
    # deleted there. Nullable so a stream can run without an overlay (raw
    # webcam test, blackhole publish, etc.). Switching the active overlay
    # mid-stream is the scene-switcher: the broadcaster re-renders without
    # tearing down the WHIP session, so latency on the wire is preserved.
    overlay_id: Mapped[uuid.UUID | None] = mapped_column(UUID(as_uuid=True), nullable=True, index=True)

    # Playlist of overlay ids the operator can swap to live. Stored as
    # ``list[str(uuid)]`` in JSONB; soft pointers like ``overlay_id``.
    # The first call to ``activate_overlay`` will refuse to set an
    # ``overlay_id`` that isn't in this list, so the macro/mobile clients
    # only switch between vetted scenes — defending against a stale id
    # that would blank the broadcast.
    overlay_playlist: Mapped[list[str]] = mapped_column(
        JSONB, nullable=False, default=list,
    )

    credential_id: Mapped[uuid.UUID] = mapped_column(
        UUID(as_uuid=True),
        ForeignKey("twitch_credentials.id", ondelete="RESTRICT"),
        nullable=False,
        index=True,
    )

    state: Mapped[StreamState] = mapped_column(
        Enum(
            StreamState,
            name="stream_state",
            values_callable=lambda enum_cls: [e.value for e in enum_cls],
        ),
        nullable=False,
        default=StreamState.PENDING,
        index=True,
    )

    # MediaMTX path name (unique per stream). Browser pushes WHIP to this path.
    mediamtx_path: Mapped[str] = mapped_column(String(128), nullable=False, unique=True)

    # WHIP endpoint returned to the browser (full URL).
    whip_endpoint: Mapped[str] = mapped_column(String(1024), nullable=False)

    # Short-lived token embedded in the WHIP URL as a query parameter.
    ingress_token: Mapped[str] = mapped_column(String(128), nullable=False, index=True)
    ingress_token_expires_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), nullable=False)

    # Populated by Twitch Helix if OAuth is connected.
    twitch_stream_id: Mapped[str | None] = mapped_column(String(64), nullable=True)

    # ---- Streaming parameters -------------------------------------------------
    # Drive the ffmpeg transcode in build_twitch_relay_config. Tweaking these on
    # an ENDED stream is fine — they're consumed at the next start_stream.
    target_width: Mapped[int] = mapped_column(Integer, nullable=False, default=1920)
    target_height: Mapped[int] = mapped_column(Integer, nullable=False, default=1080)
    target_fps: Mapped[int] = mapped_column(Integer, nullable=False, default=30)
    video_bitrate_kbps: Mapped[int] = mapped_column(Integer, nullable=False, default=6000)
    audio_bitrate_kbps: Mapped[int] = mapped_column(Integer, nullable=False, default=160)
    keyframe_interval_s: Mapped[int] = mapped_column(Integer, nullable=False, default=2)
    encoder_preset: Mapped[str] = mapped_column(String(32), nullable=False, default="veryfast")

    # When true, the ffmpeg child writes a second output to MP4 alongside
    # the RTMP push to Twitch — single transcode, two destinations via the
    # ffmpeg ``tee`` muxer. Recording lands in the ``orion_recordings``
    # named volume at ``/recordings/<stream_id>/<isodate>.mp4`` (see
    # services/mediamtx.py::build_twitch_relay_config). Read-only at
    # mid-stream — toggle only takes effect on the next start.
    record: Mapped[bool] = mapped_column(Boolean, nullable=False, default=False)

    started_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)
    ended_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)

    error_message: Mapped[str | None] = mapped_column(String(1024), nullable=True)

    # Free-form: scheduled title, game id, tags, on_start/on_end action hooks.
    metadata_: Mapped[dict[str, Any]] = mapped_column("metadata", JSONB, nullable=False, default=dict)

    created_at: Mapped[datetime] = created_at_col()
    updated_at: Mapped[datetime] = updated_at_col()
