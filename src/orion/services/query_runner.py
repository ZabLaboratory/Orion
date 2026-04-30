"""Internal QueryMe runner — the single path through which structured
read queries reach the Orion database.

Both the public HTTP surface (``POST /api/v1/_query`` in
``orion.routes.internal``) and any future internal caller go through
:func:`run_query`. That keeps :

* validation against the catalogue
* compile-error → ``CompilationError`` translation
* row serialisation (UUID / Decimal / datetime → JSON-friendly scalars)
* audit logging (``orion.queryme.audit``)

in one place. The HTTP layer only adds auth + rate limiting on top.

This module mirrors ``zabcanvas.services.query_runner`` so the two
services share identical operational behaviour for the blueprint
surface.
"""

from __future__ import annotations

import json
import logging
import time
from datetime import date, datetime
from decimal import Decimal
from typing import Any
from uuid import UUID

from queryme import (
    CompilationError,
    QueryDescriptor,
    ValidationIssue,
    compile_query,
    validate_against_schema,
)
from sqlalchemy.ext.asyncio import AsyncSession

from orion.services.db_catalog import CATALOG

audit_logger = logging.getLogger("orion.queryme.audit")


class QueryValidationError(Exception):
    """Raised when a descriptor fails catalogue validation. Carries the
    issue list so the HTTP layer can map it to a 400 response."""

    def __init__(self, issues: list[ValidationIssue]) -> None:
        super().__init__("descriptor failed catalogue validation")
        self.issues = issues


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


def _projection_keys(descriptor: QueryDescriptor) -> list[str]:
    """Return the column-name list aligned with the SQL projection.

    Single-table queries use the bare column names. When joins add
    columns that collide with the main projection, the join columns
    are prefixed with their table name so caller can disambiguate.
    """
    keys: list[str] = list(descriptor.select)
    for join in descriptor.joins:
        keys.extend(join.select)
    if len(keys) == len(set(keys)):
        return keys
    seen: set[str] = set()
    keys = []
    for col in descriptor.select:
        keys.append(col)
        seen.add(col)
    for join in descriptor.joins:
        for col in join.select:
            keys.append(col if col not in seen else f"{join.table}.{col}")
            seen.add(col)
    return keys


async def run_query(
    descriptor: QueryDescriptor,
    db: AsyncSession,
    *,
    audit_user_id: str | None = None,
) -> dict[str, Any]:
    """Validate, compile and execute ``descriptor`` against Orion.

    Raises :class:`QueryValidationError` if the descriptor doesn't
    match the catalogue, :class:`queryme.CompilationError` if the
    compilation step fails. Returns ``{rows, count, elapsed_ms}``
    with all scalars JSON-serialisable.

    ``audit_user_id`` lands in the audit log as ``user_id`` ; pass
    ``None`` for internal callers (logged as ``"internal"``).
    """
    issues = validate_against_schema(descriptor, CATALOG)
    if issues:
        raise QueryValidationError(issues)

    stmt = compile_query(descriptor, CATALOG)
    keys = _projection_keys(descriptor)

    start = time.perf_counter()
    result = await db.execute(stmt)
    rows = result.all()
    elapsed_ms = int((time.perf_counter() - start) * 1000)

    out_rows: list[dict[str, Any]] = []
    for row in rows:
        out_rows.append({keys[i]: _serialise(v) for i, v in enumerate(row)})

    audit_logger.info(
        json.dumps(
            {
                "event": "queryme.query",
                "user_id": audit_user_id or "internal",
                "table": descriptor.table,
                "joins": [j.table for j in descriptor.joins],
                "rows": len(out_rows),
                "elapsed_ms": elapsed_ms,
            }
        )
    )

    return {"rows": out_rows, "count": len(out_rows), "elapsed_ms": elapsed_ms}


__all__ = ["CompilationError", "QueryValidationError", "run_query"]
