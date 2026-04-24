"""Orion — streaming control plane microservice."""

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

from fastapi import FastAPI

from orion.database import engine
from orion.routes import chat, credentials, health, metrics, scenes, streams, twitch


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:  # pragma: no cover — ASGITransport skips lifespan
    """Hold the async engine for the lifetime of the app and dispose on exit."""
    try:
        yield
    finally:
        await engine.dispose()


app = FastAPI(
    title="Orion",
    description="Browser-composed streaming engine with Twitch relay for Zablab",
    version="0.1.0",
    lifespan=lifespan,
)

app.include_router(health.router)
app.include_router(scenes.router, prefix="/api/v1")
app.include_router(streams.router, prefix="/api/v1")
app.include_router(credentials.router, prefix="/api/v1")
app.include_router(twitch.router, prefix="/api/v1")
app.include_router(chat.router, prefix="/api/v1")
app.include_router(metrics.router, prefix="/api/v1")
