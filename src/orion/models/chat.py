"""Chat components + chat messages.

Chat components reference blueprints from an external system (declared here by string
reference — ``blueprint_ref``). The blueprint system owns the rendering logic; Orion
owns the configuration, placement, triggers, and the live chat connection.
"""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any

from sqlalchemy import BigInteger, Boolean, DateTime, ForeignKey, String, Text
from sqlalchemy.dialects.postgresql import JSONB, UUID
from sqlalchemy.orm import Mapped, mapped_column

from orion.models.base import Base, created_at_col, updated_at_col, uuid_pk


class ChatComponent(Base):
    """A chat-driven UI component that can be rendered on top of a scene.

    A blueprint-referenced component (e.g. ``chat/ticker``, ``chat/poll``,
    ``chat/command-reaction``) with owner-authored config + triggers.
    """

    __tablename__ = "chat_components"

    id: Mapped[uuid.UUID] = uuid_pk()
    owner_id: Mapped[uuid.UUID | None] = mapped_column(UUID(as_uuid=True), nullable=True, index=True)

    name: Mapped[str] = mapped_column(String(255), nullable=False)
    blueprint_ref: Mapped[str] = mapped_column(String(255), nullable=False, index=True)

    # Component-specific config (duration, style, etc.) — opaque to Orion.
    config: Mapped[dict[str, Any]] = mapped_column(JSONB, nullable=False, default=dict)

    # Where and how it renders on the scene (anchor, offset, scene slot id, etc.).
    placement: Mapped[dict[str, Any]] = mapped_column(JSONB, nullable=False, default=dict)

    # Event triggers (on_message, on_command, on_mention, on_follow, ...).
    triggers: Mapped[dict[str, Any]] = mapped_column(JSONB, nullable=False, default=dict)

    is_enabled: Mapped[bool] = mapped_column(Boolean, nullable=False, default=True)

    created_at: Mapped[datetime] = created_at_col()
    updated_at: Mapped[datetime] = updated_at_col()


class ChatMessage(Base):
    """Captured Twitch chat messages. Used for replay, history, moderation audit.

    Truncated retention is a future concern — no policy enforced at the model level.
    """

    __tablename__ = "chat_messages"

    id: Mapped[int] = mapped_column(BigInteger, primary_key=True, autoincrement=True)

    stream_id: Mapped[uuid.UUID | None] = mapped_column(
        UUID(as_uuid=True),
        ForeignKey("streams.id", ondelete="SET NULL"),
        nullable=True,
        index=True,
    )

    channel: Mapped[str] = mapped_column(String(255), nullable=False, index=True)
    author_login: Mapped[str] = mapped_column(String(255), nullable=False)
    author_id: Mapped[str | None] = mapped_column(String(64), nullable=True)
    author_display: Mapped[str | None] = mapped_column(String(255), nullable=True)
    content: Mapped[str] = mapped_column(Text, nullable=False)
    badges: Mapped[dict[str, Any]] = mapped_column(JSONB, nullable=False, default=dict)
    emotes: Mapped[dict[str, Any]] = mapped_column(JSONB, nullable=False, default=dict)

    sent_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), nullable=False)
    created_at: Mapped[datetime] = created_at_col()
