"""Stream model — lifecycle of a single broadcast session."""

from __future__ import annotations

import enum
import uuid
from datetime import datetime
from typing import Any

from sqlalchemy import DateTime, Enum, ForeignKey, String
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
    """One broadcast session binding a Scene to a TwitchCredential.

    Orion creates the MediaMTX path, hands a WHIP URL + short-lived ingress token to the
    browser, and ffmpeg inside MediaMTX pushes the RTMP feed to Twitch using the
    decrypted stream key.
    """

    __tablename__ = "streams"

    id: Mapped[uuid.UUID] = uuid_pk()
    owner_id: Mapped[uuid.UUID | None] = mapped_column(UUID(as_uuid=True), nullable=True, index=True)

    scene_id: Mapped[uuid.UUID] = mapped_column(
        UUID(as_uuid=True),
        ForeignKey("scenes.id", ondelete="RESTRICT"),
        nullable=False,
        index=True,
    )
    credential_id: Mapped[uuid.UUID] = mapped_column(
        UUID(as_uuid=True),
        ForeignKey("twitch_credentials.id", ondelete="RESTRICT"),
        nullable=False,
        index=True,
    )

    state: Mapped[StreamState] = mapped_column(
        Enum(StreamState, name="stream_state"),
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

    started_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)
    ended_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)

    error_message: Mapped[str | None] = mapped_column(String(1024), nullable=True)

    # Free-form: scheduled title, game id, tags, on_start/on_end action hooks.
    metadata_: Mapped[dict[str, Any]] = mapped_column("metadata", JSONB, nullable=False, default=dict)

    created_at: Mapped[datetime] = created_at_col()
    updated_at: Mapped[datetime] = updated_at_col()
