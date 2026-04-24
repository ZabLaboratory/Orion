"""Scene schemas."""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any

from pydantic import BaseModel, ConfigDict, Field


class SceneCreate(BaseModel):
    name: str = Field(min_length=1, max_length=255)
    description: str | None = Field(default=None, max_length=1024)
    width: int = Field(default=1920, ge=320, le=7680)
    height: int = Field(default=1080, ge=240, le=4320)
    fps: int = Field(default=30, ge=1, le=120)
    config: dict[str, Any] = Field(default_factory=dict)
    thumbnail: str | None = None


class SceneUpdate(BaseModel):
    name: str | None = Field(default=None, min_length=1, max_length=255)
    description: str | None = Field(default=None, max_length=1024)
    width: int | None = Field(default=None, ge=320, le=7680)
    height: int | None = Field(default=None, ge=240, le=4320)
    fps: int | None = Field(default=None, ge=1, le=120)
    config: dict[str, Any] | None = None
    thumbnail: str | None = None


class SceneRead(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    owner_id: uuid.UUID | None
    name: str
    description: str | None
    width: int
    height: int
    fps: int
    config: dict[str, Any]
    thumbnail: str | None
    created_at: datetime
    updated_at: datetime


class SceneSummary(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    owner_id: uuid.UUID | None
    name: str
    description: str | None
    width: int
    height: int
    fps: int
    thumbnail: str | None
    updated_at: datetime
