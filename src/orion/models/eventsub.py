"""EventSub subscriptions + event audit log.

Orion uses the Twitch EventSub **WebSocket** transport — Orion opens
an outbound WS to ``wss://eventsub.wss.twitch.tv/ws`` and Twitch
delivers events on that channel. No public HTTP endpoint required ;
HMAC verification + webhook challenge handshake are handled by the
WS handshake instead. See :mod:`orion.services.eventsub_supervisor`.

Tables :

* ``eventsub_subscriptions`` mirrors the Twitch-side subscription
  list so the operator UI can show what's wired without re-querying
  Helix on every render. The Twitch ``id`` is unique and indexed.
* ``eventsub_events`` is an audit + dedup log. Every accepted message
  lands here keyed by Twitch's ``message_id`` (unique) ; replays from
  Twitch are silently dropped on the unique-constraint conflict.
"""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any

from sqlalchemy import JSON, BigInteger, DateTime, ForeignKey, Integer, String, Uuid
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from orion.models.base import Base, created_at_col, updated_at_col, uuid_pk

# JSONB on PostgreSQL ; falls back to plain JSON on SQLite (used in tests).
JsonColumn = JSONB().with_variant(JSON(), "sqlite")


class EventSubSubscription(Base):
    """A Twitch EventSub subscription owned through one of our credentials."""

    __tablename__ = "eventsub_subscriptions"

    id: Mapped[uuid.UUID] = uuid_pk()

    credential_id: Mapped[uuid.UUID] = mapped_column(
        Uuid(as_uuid=True),
        ForeignKey("twitch_credentials.id", ondelete="CASCADE"),
        nullable=False,
        index=True,
    )

    # Twitch's own subscription id (UUID, returned by Helix). Unique so
    # we never duplicate-track. Indexed for fast lookup on event ingest.
    twitch_subscription_id: Mapped[str] = mapped_column(
        String(64), nullable=False, unique=True, index=True
    )
    event_type: Mapped[str] = mapped_column(String(64), nullable=False, index=True)
    version: Mapped[str] = mapped_column(String(8), nullable=False, default="1")

    # Twitch's status enum is open-ended (enabled, webhook_callback_verification_pending,
    # user_removed, authorization_revoked, …). Stored as free string.
    status: Mapped[str] = mapped_column(String(64), nullable=False, default="enabled")
    cost: Mapped[int] = mapped_column(Integer, nullable=False, default=1)

    # condition shape depends on event_type — broadcaster_user_id, etc. Opaque.
    condition: Mapped[dict[str, Any]] = mapped_column(JsonColumn, nullable=False, default=dict)

    created_at: Mapped[datetime] = created_at_col()
    updated_at: Mapped[datetime] = updated_at_col()


class EventSubEvent(Base):
    """Captured EventSub notification — audit + dedup."""

    __tablename__ = "eventsub_events"

    # BigInteger on PostgreSQL ; Integer on SQLite so the ROWID-backed
    # autoincrement works during tests (SQLite requires literal
    # ``INTEGER PRIMARY KEY`` for that path).
    id: Mapped[int] = mapped_column(
        BigInteger().with_variant(Integer(), "sqlite"),
        primary_key=True,
        autoincrement=True,
    )

    # Unique twitch message id — provides duplicate suppression. INSERTs
    # that race / replay surface as IntegrityError, swallowed by the
    # supervisor.
    twitch_message_id: Mapped[str] = mapped_column(
        String(128), nullable=False, unique=True, index=True
    )

    # SET NULL because Twitch can revoke subscriptions ; we still keep
    # the historical events for audit even if the subscription row is
    # gone.
    subscription_id: Mapped[uuid.UUID | None] = mapped_column(
        Uuid(as_uuid=True),
        ForeignKey("eventsub_subscriptions.id", ondelete="SET NULL"),
        nullable=True,
        index=True,
    )

    event_type: Mapped[str] = mapped_column(String(64), nullable=False, index=True)

    # Full Twitch payload. Opaque ; structure varies per event_type.
    event: Mapped[dict[str, Any]] = mapped_column(JsonColumn, nullable=False, default=dict)

    received_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), nullable=False
    )
