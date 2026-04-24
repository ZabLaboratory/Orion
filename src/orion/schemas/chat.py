"""Chat component + message schemas."""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any

from pydantic import BaseModel, ConfigDict, Field


class ChatComponentCreate(BaseModel):
    name: str = Field(min_length=1, max_length=255)
    blueprint_ref: str = Field(min_length=1, max_length=255)
    config: dict[str, Any] = Field(default_factory=dict)
    placement: dict[str, Any] = Field(default_factory=dict)
    triggers: dict[str, Any] = Field(default_factory=dict)
    is_enabled: bool = True


class ChatComponentUpdate(BaseModel):
    name: str | None = Field(default=None, min_length=1, max_length=255)
    blueprint_ref: str | None = Field(default=None, min_length=1, max_length=255)
    config: dict[str, Any] | None = None
    placement: dict[str, Any] | None = None
    triggers: dict[str, Any] | None = None
    is_enabled: bool | None = None


class ChatComponentRead(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    owner_id: uuid.UUID | None
    name: str
    blueprint_ref: str
    config: dict[str, Any]
    placement: dict[str, Any]
    triggers: dict[str, Any]
    is_enabled: bool
    created_at: datetime
    updated_at: datetime


class ChatMessageRead(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: int
    stream_id: uuid.UUID | None
    channel: str
    author_login: str
    author_id: str | None
    author_display: str | None
    content: str
    badges: dict[str, Any]
    emotes: dict[str, Any]
    sent_at: datetime
