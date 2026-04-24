"""StreamMetric — time-series snapshots polled from MediaMTX + Twitch Helix."""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any

from sqlalchemy import BigInteger, DateTime, ForeignKey, Integer
from sqlalchemy.dialects.postgresql import JSONB, UUID
from sqlalchemy.orm import Mapped, mapped_column

from orion.models.base import Base


class StreamMetric(Base):
    """A point-in-time measurement of a live stream.

    Polled by the stream_manager on a configurable interval while the stream is in the
    ``live`` state. Lightweight retention — the UI charts only recent windows.
    """

    __tablename__ = "stream_metrics"

    id: Mapped[int] = mapped_column(BigInteger, primary_key=True, autoincrement=True)

    stream_id: Mapped[uuid.UUID] = mapped_column(
        UUID(as_uuid=True),
        ForeignKey("streams.id", ondelete="CASCADE"),
        nullable=False,
        index=True,
    )

    ts: Mapped[datetime] = mapped_column(DateTime(timezone=True), nullable=False, index=True)

    bitrate_kbps: Mapped[int | None] = mapped_column(Integer, nullable=True)
    fps: Mapped[int | None] = mapped_column(Integer, nullable=True)
    dropped_frames: Mapped[int | None] = mapped_column(Integer, nullable=True)
    rtt_ms: Mapped[int | None] = mapped_column(Integer, nullable=True)
    viewers: Mapped[int | None] = mapped_column(Integer, nullable=True)

    raw: Mapped[dict[str, Any]] = mapped_column(JSONB, nullable=False, default=dict)
