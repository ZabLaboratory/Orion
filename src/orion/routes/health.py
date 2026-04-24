"""Health check endpoint."""

from __future__ import annotations

import logging

from fastapi import APIRouter, Depends
from fastapi.responses import JSONResponse
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session

logger = logging.getLogger(__name__)

router = APIRouter()


async def _check_database(db: AsyncSession) -> bool:
    try:
        await db.execute(text("SELECT 1"))
        return True
    except Exception:
        logger.exception("Database health check failed")
        return False


@router.get("/health")
async def health(db: AsyncSession = Depends(get_session)) -> JSONResponse:
    """Liveness probe with database connectivity check."""
    db_ok = await _check_database(db)

    if db_ok:
        return JSONResponse(
            status_code=200,
            content={
                "status": "ok",
                "service": "orion",
                "database": "connected",
            },
        )

    return JSONResponse(
        status_code=503,
        content={
            "status": "degraded",
            "service": "orion",
            "database": "disconnected",
        },
    )
