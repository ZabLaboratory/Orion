"""Twitch credential schemas — secrets never leak outbound."""

from __future__ import annotations

import uuid
from datetime import datetime

from pydantic import BaseModel, ConfigDict, Field


class CredentialCreate(BaseModel):
    """Payload for registering a Twitch destination. Stream key is write-only."""

    label: str = Field(min_length=1, max_length=255)
    stream_key: str = Field(min_length=1, max_length=512)
    channel_login: str | None = Field(default=None, max_length=255)
    channel_id: str | None = Field(default=None, max_length=64)


class CredentialUpdate(BaseModel):
    label: str | None = Field(default=None, min_length=1, max_length=255)
    stream_key: str | None = Field(default=None, min_length=1, max_length=512)
    channel_login: str | None = Field(default=None, max_length=255)
    channel_id: str | None = Field(default=None, max_length=64)


class CredentialRead(BaseModel):
    """Outbound credential. Secrets are never exposed — only presence flags."""

    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    owner_id: uuid.UUID
    label: str
    channel_login: str | None
    channel_id: str | None
    has_oauth: bool
    oauth_expires_at: datetime | None
    oauth_scopes: list[str] | None
    created_at: datetime
    updated_at: datetime
