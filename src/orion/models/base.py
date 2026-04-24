"""Declarative base with shared column helpers."""

import uuid
from datetime import datetime

from sqlalchemy import DateTime, func
from sqlalchemy.dialects.postgresql import UUID
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column


class Base(DeclarativeBase):
    """Shared declarative base for all Orion ORM models."""


def uuid_pk() -> Mapped[uuid.UUID]:
    """Primary key column: UUID, server-default gen_random_uuid()."""
    return mapped_column(
        UUID(as_uuid=True),
        primary_key=True,
        server_default=func.gen_random_uuid(),
    )


def created_at_col() -> Mapped[datetime]:
    """Server-managed created_at timestamp."""
    return mapped_column(
        DateTime(timezone=True),
        server_default=func.now(),
        nullable=False,
    )


def updated_at_col() -> Mapped[datetime]:
    """Server-managed updated_at timestamp — refreshed on every UPDATE."""
    return mapped_column(
        DateTime(timezone=True),
        server_default=func.now(),
        onupdate=func.now(),
        nullable=False,
    )
