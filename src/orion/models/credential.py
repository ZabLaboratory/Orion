"""TwitchCredential — encrypted stream key + optional OAuth tokens."""

from __future__ import annotations

import uuid
from datetime import datetime

from sqlalchemy import ARRAY, DateTime, LargeBinary, String
from sqlalchemy.dialects.postgresql import UUID
from sqlalchemy.orm import Mapped, mapped_column

from orion.models.base import Base, created_at_col, updated_at_col, uuid_pk


class TwitchCredential(Base):
    """A user's Twitch destination.

    - ``stream_key_*`` is always present (needed to push RTMP to Twitch).
    - ``oauth_*`` fields are optional; populated via the OAuth flow to enable
      channel metadata updates (Helix) and chat connection (IRC).
    - Secrets are AES-GCM encrypted with ``settings.encryption_key``.
    """

    __tablename__ = "twitch_credentials"

    id: Mapped[uuid.UUID] = uuid_pk()
    owner_id: Mapped[uuid.UUID | None] = mapped_column(UUID(as_uuid=True), nullable=True, index=True)

    label: Mapped[str] = mapped_column(String(255), nullable=False)

    channel_login: Mapped[str | None] = mapped_column(String(255), nullable=True)
    channel_id: Mapped[str | None] = mapped_column(String(64), nullable=True)

    stream_key_ciphertext: Mapped[bytes] = mapped_column(LargeBinary, nullable=False)
    stream_key_nonce: Mapped[bytes] = mapped_column(LargeBinary, nullable=False)

    oauth_access_ciphertext: Mapped[bytes | None] = mapped_column(LargeBinary, nullable=True)
    oauth_access_nonce: Mapped[bytes | None] = mapped_column(LargeBinary, nullable=True)
    oauth_refresh_ciphertext: Mapped[bytes | None] = mapped_column(LargeBinary, nullable=True)
    oauth_refresh_nonce: Mapped[bytes | None] = mapped_column(LargeBinary, nullable=True)
    oauth_expires_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), nullable=True)
    oauth_scopes: Mapped[list[str] | None] = mapped_column(ARRAY(String(64)), nullable=True)

    created_at: Mapped[datetime] = created_at_col()
    updated_at: Mapped[datetime] = updated_at_col()
