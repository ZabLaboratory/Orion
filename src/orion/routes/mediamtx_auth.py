"""MediaMTX external authentication webhook.

MediaMTX calls this endpoint for every publisher / reader action when configured
with ``authMethod: http``. We verify that the ingress token carried in the WHIP
URL's query string matches a stream row and has not expired.

MediaMTX POSTs a payload shaped like::

    {
      "user": "",
      "password": "",
      "ip": "...",
      "action": "publish" | "read" | ...,
      "path": "orion_<hex>",
      "protocol": "webrtc",
      "id": "...",
      "query": "token=<ingress_token>"
    }

We only gate publish actions — reads (HLS playback, observability) stay permissive.
"""

from __future__ import annotations

import logging
from datetime import UTC, datetime
from typing import Literal
from urllib.parse import parse_qs

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.models.stream import StreamState
from orion.services import stream_manager

logger = logging.getLogger(__name__)

router = APIRouter(prefix="/mediamtx", tags=["mediamtx"])


class MediaMTXAuthRequest(BaseModel):
    user: str = ""
    password: str = ""
    ip: str = ""
    action: Literal["publish", "read", "playback", "api", "metrics", "pprof"] | str
    path: str
    protocol: str | None = None
    id: str | None = None
    query: str = ""


@router.post("/auth", status_code=status.HTTP_200_OK)
async def mediamtx_auth(
    payload: MediaMTXAuthRequest,
    db: AsyncSession = Depends(get_session),
) -> dict[str, str]:
    """Accept or reject a MediaMTX action.

    Returns 200 on success, 401 on refusal. MediaMTX interprets any non-2xx as a
    denial.
    """
    # Reads are open — MediaMTX playback (HLS/WebRTC sub) doesn't need Orion auth
    # since paths already have unguessable UUID names. Tighten here if needed.
    if payload.action != "publish":
        return {"status": "ok"}

    qs = parse_qs(payload.query)
    token_values = qs.get("token", [])
    if not token_values:
        logger.warning("MediaMTX publish denied: no token in query (path=%s)", payload.path)
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, detail="Missing ingress token.")

    token = token_values[0]
    stream = await stream_manager.get_stream_by_ingress_token(db, token)
    if stream is None:
        logger.warning("MediaMTX publish denied: token not found (path=%s)", payload.path)
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, detail="Unknown ingress token.")

    if stream.mediamtx_path != payload.path:
        logger.warning(
            "MediaMTX publish denied: path mismatch (token path=%s, mtx path=%s)",
            stream.mediamtx_path,
            payload.path,
        )
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, detail="Token/path mismatch.")

    if stream.ingress_token_expires_at < datetime.now(tz=UTC):
        logger.warning("MediaMTX publish denied: token expired (stream=%s)", stream.id)
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, detail="Ingress token expired.")

    if stream.state not in (StreamState.PREPARING, StreamState.LIVE):
        logger.warning(
            "MediaMTX publish denied: stream state=%s (stream=%s)",
            stream.state.value,
            stream.id,
        )
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, detail="Stream not ready to ingest.")

    # First publish on a PREPARING stream flips it to LIVE.
    if stream.state == StreamState.PREPARING:
        await stream_manager.mark_live(db, stream)
        await db.commit()

    return {"status": "ok", "stream_id": str(stream.id)}
