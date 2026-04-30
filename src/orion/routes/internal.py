"""Internal blueprint-facing endpoints — read-only schema + query.

Per ADR 001 §1, Orion ships ``_schema`` + ``_query`` only. Credential
rotation and OAuth flow stay on their dedicated routes so the
encryption boundaries are preserved.

This module is HTTP-only : auth (``X-Authenticated-User`` enforcement),
rate limiting (per-user, in-memory per worker), then it hands the
descriptor over to :func:`orion.services.query_runner.run_query`. The
runner owns validation, compilation, execution, serialisation and the
audit log so that internal callers route through the same path.
``_schema`` stays public — the catalogue itself carries no rows, and
Orion's catalogue is intentionally narrow (``chat_messages`` only ;
``twitch_credentials`` is omitted entirely because every column is
sensitive).
"""

from __future__ import annotations

import time
from collections import defaultdict, deque
from threading import Lock
from typing import Annotated, Any
from uuid import UUID

from fastapi import APIRouter, Depends, HTTPException, status
from queryme import CompilationError, QueryDescriptor, SchemaDescriptor
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.routes._deps import require_authenticated_user
from orion.services import query_runner
from orion.services.db_catalog import CATALOG

router = APIRouter(tags=["internal"])

# Sliding window rate limit — per-user, per-process. Tunable from tests
# via monkeypatch on these module-level constants.
RATE_LIMIT_WINDOW_SECONDS: float = 60.0
RATE_LIMIT_MAX_REQUESTS: int = 120

_rate_buckets: dict[str, deque[float]] = defaultdict(deque)
_rate_lock = Lock()


def _check_rate_limit(user_id: str) -> None:
    now = time.monotonic()
    cutoff = now - RATE_LIMIT_WINDOW_SECONDS
    with _rate_lock:
        bucket = _rate_buckets[user_id]
        while bucket and bucket[0] < cutoff:
            bucket.popleft()
        if len(bucket) >= RATE_LIMIT_MAX_REQUESTS:
            retry_in = int(bucket[0] + RATE_LIMIT_WINDOW_SECONDS - now) + 1
            raise HTTPException(
                status_code=status.HTTP_429_TOO_MANY_REQUESTS,
                detail={"retry_after_seconds": retry_in},
                headers={"Retry-After": str(retry_in)},
            )
        bucket.append(now)


@router.get("/_schema", response_model=SchemaDescriptor)
async def get_schema() -> SchemaDescriptor:
    return CATALOG


@router.post("/_query", status_code=status.HTTP_200_OK)
async def execute_query(
    descriptor: QueryDescriptor,
    user_id: Annotated[UUID, Depends(require_authenticated_user)],
    db: Annotated[AsyncSession, Depends(get_session)],
) -> dict[str, Any]:
    _check_rate_limit(str(user_id))
    try:
        return await query_runner.run_query(descriptor, db, audit_user_id=str(user_id))
    except query_runner.QueryValidationError as exc:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"issues": [i.model_dump() for i in exc.issues]},
        ) from exc
    except CompilationError as exc:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"issues": [i.model_dump() for i in exc.issues]},
        ) from exc
