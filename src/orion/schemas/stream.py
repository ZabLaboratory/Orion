"""Stream schemas."""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any

from pydantic import BaseModel, ConfigDict, Field

from orion.models.stream import StreamState


class StreamCreate(BaseModel):
    scene_id: uuid.UUID
    credential_id: uuid.UUID
    # Optional title/game/tags set on Twitch channel via Helix if OAuth is connected.
    title: str | None = Field(default=None, max_length=140)
    game_id: str | None = Field(default=None, max_length=64)
    tags: list[str] = Field(default_factory=list)
    metadata: dict[str, Any] = Field(default_factory=dict)


class StreamRead(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    owner_id: uuid.UUID | None
    scene_id: uuid.UUID
    credential_id: uuid.UUID
    state: StreamState
    mediamtx_path: str
    whip_endpoint: str
    ingress_token_expires_at: datetime
    twitch_stream_id: str | None
    started_at: datetime | None
    ended_at: datetime | None
    error_message: str | None
    metadata: dict[str, Any] = Field(validation_alias="metadata_")
    created_at: datetime
    updated_at: datetime


class StreamSummary(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    scene_id: uuid.UUID
    credential_id: uuid.UUID
    state: StreamState
    started_at: datetime | None
    ended_at: datetime | None
    updated_at: datetime


class StreamStartResponse(BaseModel):
    """Handed back to the browser when a stream is started — contains the WHIP target."""

    stream: StreamRead
    whip_url: str  # full URL the browser sends its SDP offer to
    ice_servers: list[dict[str, Any]] = Field(default_factory=list)
