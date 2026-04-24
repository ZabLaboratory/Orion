"""Stream metric schemas."""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any

from pydantic import BaseModel, ConfigDict


class StreamMetricRead(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: int
    stream_id: uuid.UUID
    ts: datetime
    bitrate_kbps: int | None
    fps: int | None
    dropped_frames: int | None
    rtt_ms: int | None
    viewers: int | None
    raw: dict[str, Any]
