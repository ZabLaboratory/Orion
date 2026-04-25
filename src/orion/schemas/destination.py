"""StreamDestination schemas — secrets stay server-side, the API only
returns metadata + a `kind` label. Plaintext rtmp_url + stream_key
are accepted on create / update and immediately AES-GCM encrypted
before they reach disk.
"""

from __future__ import annotations

import uuid
from datetime import datetime

from pydantic import BaseModel, ConfigDict, Field


class StreamDestinationCreate(BaseModel):
    """Create payload — plaintext secrets accepted only on the wire,
    encrypted at rest immediately. The browser is encouraged to send
    the URL/key once at registration and never re-fetch them."""

    kind: str = Field(default="custom-rtmp", max_length=32)
    display_name: str = Field(max_length=128)
    rtmp_url: str = Field(max_length=1024, description="e.g. rtmp://live.twitch.tv/app/")
    stream_key: str = Field(max_length=512)
    enabled: bool = True
    ordering: int = 0


class StreamDestinationUpdate(BaseModel):
    """Partial update — pass any subset. Secrets stay encrypted at rest;
    if either ``rtmp_url`` or ``stream_key`` is provided, that one is
    re-encrypted while the other is left alone."""

    kind: str | None = Field(default=None, max_length=32)
    display_name: str | None = Field(default=None, max_length=128)
    rtmp_url: str | None = Field(default=None, max_length=1024)
    stream_key: str | None = Field(default=None, max_length=512)
    enabled: bool | None = None
    ordering: int | None = None


class StreamDestinationRead(BaseModel):
    """Read view — secrets are NEVER exposed. ``has_credentials`` is
    always true for now (both columns are NOT NULL); kept as an
    explicit flag so a future "blank destination, fill on next start"
    flow doesn't change the response shape."""

    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    stream_id: uuid.UUID
    kind: str
    display_name: str
    enabled: bool
    ordering: int
    created_at: datetime
    updated_at: datetime
