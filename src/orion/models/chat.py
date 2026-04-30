"""Captured Twitch chat messages — kept for replay / audit / blueprint history.

The "chat component" abstraction lives in ZabCanvas now: a chat-driven UI is a
scene component (`type: "blueprint"`) whose Blue binding subscribes to the
chat events bus. Orion only owns the IRC pump (it has the OAuth tokens) and
this lightweight transcript table.

Messages are channel-keyed only. Streaming sessions are external (Pulsar in
Prism) so there is no internal stream identity to bind to ; analytics group
by ``channel`` and ``sent_at`` instead.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

from sqlalchemy import JSON, BigInteger, DateTime, String, Text
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from orion.models.base import Base, created_at_col

# JSONB on PostgreSQL ; falls back to plain JSON on SQLite (used in tests).
JsonColumn = JSONB().with_variant(JSON(), "sqlite")


class ChatMessage(Base):
    """Captured Twitch chat messages. Used for replay, history, moderation audit.

    Retention truncation is a future concern — no policy enforced at the model level.
    """

    __tablename__ = "chat_messages"

    id: Mapped[int] = mapped_column(BigInteger, primary_key=True, autoincrement=True)

    channel: Mapped[str] = mapped_column(String(255), nullable=False, index=True)
    author_login: Mapped[str] = mapped_column(String(255), nullable=False)
    author_id: Mapped[str | None] = mapped_column(String(64), nullable=True)
    author_display: Mapped[str | None] = mapped_column(String(255), nullable=True)
    content: Mapped[str] = mapped_column(Text, nullable=False)
    badges: Mapped[dict[str, Any]] = mapped_column(JsonColumn, nullable=False, default=dict)
    emotes: Mapped[dict[str, Any]] = mapped_column(JsonColumn, nullable=False, default=dict)

    sent_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), nullable=False)
    created_at: Mapped[datetime] = created_at_col()
