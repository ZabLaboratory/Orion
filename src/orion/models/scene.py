"""Scene model — browser-composable broadcast scene."""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any

from sqlalchemy import Integer, String
from sqlalchemy.dialects.postgresql import JSONB, UUID
from sqlalchemy.orm import Mapped, mapped_column

from orion.models.base import Base, created_at_col, updated_at_col, uuid_pk


class Scene(Base):
    """A user-authored scene: layout, sources, styles. Rendered on canvas in the browser.

    The ``config`` blob is intentionally schema-less on the DB side — the editor owns the
    exact component shape. Backend persists and returns it verbatim. Forward-compatible
    with new source/component types without touching migrations.

    Expected ``config`` shape (indicative, not enforced):
        {
          "width": 1920, "height": 1080, "fps": 30,
          "sources": [
            {"id": "...", "type": "webcam"|"screen"|"image"|"text"|"iframe"|"canvas-overlay",
             "placement": {"x": 0, "y": 0, "w": 1920, "h": 1080, "z": 0},
             "config": {...}},
            ...
          ],
          "audio": {"mic": {...}, "system": {...}, "mixer": [...]},
          "twitch": {"target_bitrate_kbps": 6000, "keyframe_interval_s": 2}
        }
    """

    __tablename__ = "scenes"

    id: Mapped[uuid.UUID] = uuid_pk()
    owner_id: Mapped[uuid.UUID | None] = mapped_column(UUID(as_uuid=True), nullable=True, index=True)

    name: Mapped[str] = mapped_column(String(255), nullable=False)
    description: Mapped[str | None] = mapped_column(String(1024), nullable=True)

    width: Mapped[int] = mapped_column(Integer, nullable=False, default=1920)
    height: Mapped[int] = mapped_column(Integer, nullable=False, default=1080)
    fps: Mapped[int] = mapped_column(Integer, nullable=False, default=30)

    config: Mapped[dict[str, Any]] = mapped_column(JSONB, nullable=False, default=dict)

    thumbnail: Mapped[str | None] = mapped_column(String(4096), nullable=True)

    created_at: Mapped[datetime] = created_at_col()
    updated_at: Mapped[datetime] = updated_at_col()
