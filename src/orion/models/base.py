"""Declarative base with shared column helpers."""

import uuid
from datetime import datetime

from sqlalchemy import DateTime, Uuid, func
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column


class Base(DeclarativeBase):
    """Shared declarative base for all Orion ORM models."""


def uuid_pk() -> Mapped[uuid.UUID]:
    """Primary key column: UUID. Python-side default (``uuid4``) plus a
    PostgreSQL server-side fallback (``gen_random_uuid()``) for any
    raw SQL insert that bypasses the ORM. The Python default keeps
    the SQLite test backend happy — it has no ``gen_random_uuid()``."""
    return mapped_column(
        Uuid(as_uuid=True),
        primary_key=True,
        default=uuid.uuid4,
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
