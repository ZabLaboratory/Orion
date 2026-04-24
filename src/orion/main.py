"""Orion — streaming control plane microservice."""

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

from fastapi import FastAPI

from orion.database import engine
from orion.routes import chat, credentials, health, mediamtx_auth, metrics, scenes, streams, twitch
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
    description="Browser-composed streaming engine with Twitch relay for Zablab",
    version="0.1.0",
    lifespan=lifespan,
)

app.include_router(health.router)
# MediaMTX webhook lives OUTSIDE /api/v1 — MediaMTX is an infrastructure peer,
# not an API consumer. This path is on the internal network only.
app.include_router(mediamtx_auth.router)
app.include_router(scenes.router, prefix="/api/v1")
app.include_router(streams.router, prefix="/api/v1")
app.include_router(credentials.router, prefix="/api/v1")
app.include_router(twitch.router, prefix="/api/v1")
app.include_router(chat.router, prefix="/api/v1")
app.include_router(metrics.router, prefix="/api/v1")
