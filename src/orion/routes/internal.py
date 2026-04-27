"""Internal blueprint-facing endpoints — read-only schema + query.

Per ADR 001 §1, Orion ships ``_schema`` + ``_query`` only. Stream
lifecycle management (``start`` / ``stop``) and credential rotation
stay on their dedicated routes so the single-writer-on-state rule
on ``stream_manager`` and the encryption boundaries are preserved.
"""

from __future__ import annotations

import time
from datetime import date, datetime
from decimal import Decimal
from typing import Any
from uuid import UUID

from fastapi import APIRouter, Depends, HTTPException, status
from queryme import (
    CompilationError,
    QueryDescriptor,
    SchemaDescriptor,
    compile_query,
    validate_against_schema,
)
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.services.db_catalog import CATALOG

router = APIRouter(tags=["internal"])


@router.get("/_schema", response_model=SchemaDescriptor)
async def get_schema() -> SchemaDescriptor:
    return CATALOG


def _serialise(value: Any) -> Any:  # noqa: ANN401 — opaque DB scalar
    if isinstance(value, UUID):
        return str(value)
    if isinstance(value, Decimal):
        return float(value)
    if isinstance(value, (datetime, date)):
        return value.isoformat()
    if isinstance(value, bytes):
        # Defensive : binary columns aren't in the catalog but should
        # something slip through, do not echo raw bytes.
        return None
    return value


@router.post("/_query", status_code=status.HTTP_200_OK)
async def execute_query(
    descriptor: QueryDescriptor,
    db: AsyncSession = Depends(get_session),
) -> dict[str, Any]:
    issues = validate_against_schema(descriptor, CATALOG)
    if issues:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"issues": [i.model_dump() for i in issues]},
        )

    try:
        stmt = compile_query(descriptor, CATALOG)
    except CompilationError as exc:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={"issues": [i.model_dump() for i in exc.issues]},
        ) from exc

    keys: list[str] = list(descriptor.select)
    for join in descriptor.joins:
        keys.extend(join.select)
    if len(keys) != len(set(keys)):
        seen: set[str] = set()
        keys = []
        for col in descriptor.select:
            keys.append(col)
            seen.add(col)
        for join in descriptor.joins:
            for col in join.select:
                keys.append(col if col not in seen else f"{join.table}.{col}")
                seen.add(col)

    start = time.perf_counter()
    result = await db.execute(stmt)
    rows = result.all()
    elapsed_ms = int((time.perf_counter() - start) * 1000)

    out_rows: list[dict[str, Any]] = []
    for row in rows:
        out_rows.append({keys[i]: _serialise(v) for i, v in enumerate(row)})

    return {"rows": out_rows, "count": len(out_rows), "elapsed_ms": elapsed_ms}
