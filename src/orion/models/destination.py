"""StreamDestination — additional RTMP/SRT outputs attached to a stream.

The legacy single-Twitch path (``Stream.credential_id`` →
``TwitchCredential``) stays untouched: a stream with no destinations
keeps streaming to its TwitchCredential alone. When at least one
``StreamDestination`` is attached to a stream, the runOnReady ffmpeg
fan-outs to each enabled destination via the ffmpeg ``tee`` muxer in
addition to (NOT instead of) the legacy Twitch path.

This unblocks Twitch + YouTube simultaneous streaming without
forcing users off the existing Twitch-only flow. Future destinations
(Facebook Live, custom RTMP) plug in the same way — they all speak
RTMP and ffmpeg doesn't care which platform is at the other end.
"""

from __future__ import annotations

import uuid
from datetime import datetime

from sqlalchemy import Boolean, ForeignKey, Integer, LargeBinary, String
from sqlalchemy.dialects.postgresql import UUID
from sqlalchemy.orm import Mapped, mapped_column

from orion.models.base import Base, created_at_col, updated_at_col, uuid_pk


class StreamDestination(Base):
    """An additional RTMP destination for a stream.

    Both ``rtmp_url`` and ``stream_key`` are AES-GCM encrypted with
    ``settings.encryption_key`` because either alone is enough to take
    over the destination — neither belongs in plaintext anywhere on
    disk or in logs.

    The ``kind`` field is a free-form label for UI purposes (twitch /
    youtube / facebook / custom-rtmp). Orion's runtime does not branch
    on it; ffmpeg pushes RTMP regardless. Helix-style platform
    integrations (chat, scheduled-title updates) live separately and
    are tied to ``TwitchCredential`` only — adding YouTube Helix
    equivalents would be its own scoped work.
    """

    __tablename__ = "stream_destinations"

    id: Mapped[uuid.UUID] = uuid_pk()
    stream_id: Mapped[uuid.UUID] = mapped_column(
        UUID(as_uuid=True),
        ForeignKey("streams.id", ondelete="CASCADE"),
        nullable=False,
        index=True,
    )

    kind: Mapped[str] = mapped_column(String(32), nullable=False, default="custom-rtmp")
    display_name: Mapped[str] = mapped_column(String(128), nullable=False)

    rtmp_url_ciphertext: Mapped[bytes] = mapped_column(LargeBinary, nullable=False)
    rtmp_url_nonce: Mapped[bytes] = mapped_column(LargeBinary, nullable=False)
    stream_key_ciphertext: Mapped[bytes] = mapped_column(LargeBinary, nullable=False)
    stream_key_nonce: Mapped[bytes] = mapped_column(LargeBinary, nullable=False)

    enabled: Mapped[bool] = mapped_column(Boolean, nullable=False, default=True)
    # Display-only ordering for the UI. Doesn't affect ffmpeg tee output —
    # all enabled destinations get one branch each, parallel.
    ordering: Mapped[int] = mapped_column(Integer, nullable=False, default=0)

    created_at: Mapped[datetime] = created_at_col()
    updated_at: Mapped[datetime] = updated_at_col()
