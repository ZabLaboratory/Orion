"""Shared route helpers — auth header parsing, MediaMTX client dependency."""

from __future__ import annotations

import logging
import uuid
from collections.abc import AsyncIterator

from fastapi import Depends, HTTPException, Request, status

from orion.services.mediamtx import MediaMTXClient

logger = logging.getLogger(__name__)


def authenticated_user(request: Request) -> uuid.UUID | None:
    """Read the user ID injected by ZabGate after JWT validation.

    Returns None when the header is absent — some Orion endpoints are callable
    anonymously during scaffolding. Routes that require an identity should use
    ``require_authenticated_user`` instead.
    """
    raw = request.headers.get("x-authenticated-user")
    if not raw:
        return None
    try:
        return uuid.UUID(raw)
    except ValueError:
        logger.warning("Invalid X-Authenticated-User header value: %r", raw)
        return None


def require_authenticated_user(
    user_id: uuid.UUID | None = Depends(authenticated_user),
) -> uuid.UUID:
    if user_id is None:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Authentication required.",
        )
    return user_id


def authenticated_role(request: Request) -> str | None:
    return request.headers.get("x-authenticated-role")


async def mediamtx_client() -> AsyncIterator[MediaMTXClient]:
    """Per-request MediaMTX client. Closes on response teardown."""
    client = MediaMTXClient()
    try:
        yield client
    finally:
        await client.aclose()
