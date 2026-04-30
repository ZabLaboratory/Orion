"""Orion — Twitch orchestrator microservice.

Orion is the Zablab platform's interface to Twitch. It owns :

- Twitch credentials (AES-GCM encrypted stream keys + optional Helix OAuth)
- Helix OAuth flow (authorize / callback)
- IRC chat plumbing (capture + WS fan-out for blueprint consumers)

It does **not** own broadcast media. Streaming is handled externally by
Pulsar (broadcast engine bundled in Prism), which pushes directly to
Twitch RTMP. Pulsar is an external module — Orion never touches it,
and Pulsar never touches Orion's infrastructure.

Future scope (separate PRs) :

- EventSub webhooks (subs / donations / bits / follows / raids / hype)
- Expanded Helix endpoints (channel info, schedule, clips, predictions)
- Subscriber-driven IRC supervisor (re-introduces chat capture when a
  WS client subscribes to ``/api/v1/chat/live/{channel}``)
"""

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

from fastapi import FastAPI

from orion.database import engine
from orion.routes import (
    chat,
    credentials,
    eventsub,
    health,
    internal,
    twitch,
)
from orion.services.eventsub_supervisor import supervisor as eventsub_supervisor
from orion.services.irc_supervisor import supervisor as irc_supervisor


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:  # pragma: no cover — ASGITransport skips lifespan
    """Hold the async engine for the lifetime of the app, and tear
    down the IRC fleet + EventSub WS connections opened on demand."""
    try:
        yield
    finally:
        await irc_supervisor.shutdown()
        await eventsub_supervisor.shutdown()
        await engine.dispose()


app = FastAPI(
    title="Orion",
    description=(
        "Twitch orchestrator for Zablab — credentials, OAuth, chat. "
        "Streaming media lives elsewhere (Pulsar in Prism)."
    ),
    version="0.4.0",
    lifespan=lifespan,
)

app.include_router(health.router)
app.include_router(credentials.router, prefix="/api/v1")
app.include_router(twitch.router, prefix="/api/v1")
app.include_router(chat.router, prefix="/api/v1")
app.include_router(eventsub.router, prefix="/api/v1")
app.include_router(internal.router, prefix="/api/v1")
