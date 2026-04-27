"""Orion — streaming control plane microservice.

Orion does not author scenes. Scenes live in ZabCanvas (visual editor +
JSONB blob, blueprint-backed components resolved by Blue at render time).
Orion owns:
  - Twitch credentials (encrypted stream keys + OAuth tokens)
  - Stream sessions (lifecycle + streaming parameters that drive ffmpeg)
  - MediaMTX orchestration (WHIP ingress → RTMP push to Twitch)
  - Twitch IRC pump + chat events bus (Blue blueprints subscribe over WS)
"""

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

from fastapi import FastAPI

from orion.database import engine
from orion.routes import (
    chat,
    credentials,
    destinations,
    health,
    internal,
    mediamtx_auth,
    metrics,
    streams,
    twitch,
)
from orion.services.chat_supervisor import supervisor as chat_supervisor


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:  # pragma: no cover — ASGITransport skips lifespan
    """Hold the async engine + chat supervisor for the lifetime of the app."""
    await chat_supervisor.start()
    try:
        yield
    finally:
        await chat_supervisor.stop()
        await engine.dispose()


app = FastAPI(
    title="Orion",
    description=(
        "Streaming control plane for Zablab — overlays come from ZabCanvas, "
        "blueprints from Blue, video relays via MediaMTX to Twitch"
    ),
    version="0.2.0",
    lifespan=lifespan,
)

app.include_router(health.router)
# MediaMTX webhook lives OUTSIDE /api/v1 — MediaMTX is an infrastructure peer,
# not an API consumer. This path is on the internal network only.
app.include_router(mediamtx_auth.router)
app.include_router(streams.router, prefix="/api/v1")
app.include_router(destinations.router, prefix="/api/v1")
app.include_router(credentials.router, prefix="/api/v1")
app.include_router(twitch.router, prefix="/api/v1")
app.include_router(chat.router, prefix="/api/v1")
app.include_router(metrics.router, prefix="/api/v1")
app.include_router(internal.router, prefix="/api/v1")
