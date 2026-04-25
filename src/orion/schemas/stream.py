"""Stream schemas."""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any

from pydantic import BaseModel, ConfigDict, Field

from orion.models.stream import StreamState


class StreamCreate(BaseModel):
    """Payload for `POST /streams`.

    ``overlay_id`` is a soft pointer into ZabCanvas — Orion stores it but
    never dereferences it; the broadcaster (Prism / ZabView) is the one that
    fetches the overlay and resolves its blueprints.
    """

    overlay_id: uuid.UUID | None = None
    overlay_playlist: list[uuid.UUID] = Field(default_factory=list)
    credential_id: uuid.UUID

    # Streaming parameters consumed by ffmpeg at start_stream time. All
    # optional; defaults match Twitch Partner-tier 1080p30 6 Mbps.
    target_width: int = Field(default=1920, ge=320, le=3840)
    target_height: int = Field(default=1080, ge=240, le=2160)
    target_fps: int = Field(default=30, ge=1, le=120)
    video_bitrate_kbps: int = Field(default=6000, ge=500, le=20000)
    audio_bitrate_kbps: int = Field(default=160, ge=64, le=320)
    keyframe_interval_s: int = Field(default=2, ge=1, le=10)
    encoder_preset: str = Field(default="veryfast", max_length=32)

    # Optional title/game/tags set on Twitch channel via Helix if OAuth is connected.
    title: str | None = Field(default=None, max_length=140)
    game_id: str | None = Field(default=None, max_length=64)
    tags: list[str] = Field(default_factory=list)
    metadata: dict[str, Any] = Field(default_factory=dict)


class StreamUpdate(BaseModel):
    """Patch streaming parameters or the overlay reference on an existing stream.

    Only writeable while the stream is not LIVE — applied at the next
    `start_stream`. Tweak resolution/bitrate between sessions, swap overlays,
    etc.
    """

    overlay_id: uuid.UUID | None = None
    overlay_playlist: list[uuid.UUID] | None = None
    target_width: int | None = Field(default=None, ge=320, le=3840)
    target_height: int | None = Field(default=None, ge=240, le=2160)
    target_fps: int | None = Field(default=None, ge=1, le=120)
    video_bitrate_kbps: int | None = Field(default=None, ge=500, le=20000)
    audio_bitrate_kbps: int | None = Field(default=None, ge=64, le=320)
    keyframe_interval_s: int | None = Field(default=None, ge=1, le=10)
    encoder_preset: str | None = Field(default=None, max_length=32)
    metadata: dict[str, Any] | None = None


class StreamRead(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    owner_id: uuid.UUID | None
    overlay_id: uuid.UUID | None
    overlay_playlist: list[uuid.UUID] = Field(default_factory=list)
    credential_id: uuid.UUID
    state: StreamState
    mediamtx_path: str
    whip_endpoint: str
    ingress_token_expires_at: datetime
    twitch_stream_id: str | None
    target_width: int
    target_height: int
    target_fps: int
    video_bitrate_kbps: int
    audio_bitrate_kbps: int
    keyframe_interval_s: int
    encoder_preset: str
    started_at: datetime | None
    ended_at: datetime | None
    error_message: str | None
    metadata: dict[str, Any] = Field(validation_alias="metadata_")
    created_at: datetime
    updated_at: datetime


class StreamSummary(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    overlay_id: uuid.UUID | None
    overlay_playlist: list[uuid.UUID] = Field(default_factory=list)
    credential_id: uuid.UUID
    state: StreamState
    target_width: int
    target_height: int
    target_fps: int
    video_bitrate_kbps: int
    started_at: datetime | None
    ended_at: datetime | None
    updated_at: datetime


class StreamStartResponse(BaseModel):
    """Handed back to the browser when a stream is started — contains the WHIP target."""

    stream: StreamRead
    whip_url: str  # full URL the browser sends its SDP offer to
    ice_servers: list[dict[str, Any]] = Field(default_factory=list)


class ActivateOverlayRequest(BaseModel):
    """``POST /streams/{id}/active-overlay`` body — switch the live
    overlay to one already in the stream's ``overlay_playlist``. The
    server-side check refuses ids outside the playlist so a stale or
    rogue request can't blank the broadcast. ``None`` clears the
    active overlay (raw camera fallback)."""

    overlay_id: uuid.UUID | None = None
