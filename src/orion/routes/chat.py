"""Chat live WebSocket fan-out.

Chat *components* live in ZabCanvas as scene components (Blue blueprints
resolved at render time). Orion only owns the IRC pump (it has the OAuth
tokens) and this WebSocket — Blue's chat-events nodes subscribe here.

The IRC connection is opened on demand when the first WS subscriber
arrives for a channel, and closed after a grace window when the last
one leaves. See :mod:`orion.services.irc_supervisor`.
"""

from __future__ import annotations

from fastapi import APIRouter, WebSocket, WebSocketDisconnect

from orion.services.chat_service import chat_bus
from orion.services.irc_supervisor import supervisor as irc_supervisor

router = APIRouter(prefix="/chat", tags=["chat"])


@router.websocket("/live/{channel_login}")
async def chat_live_socket(websocket: WebSocket, channel_login: str) -> None:
    """Stream Twitch IRC events for a channel as JSON frames.

    On connect the supervisor is asked to bring an IRC client up for
    ``channel_login`` (idempotent — re-uses an existing client if one
    is already running). On disconnect the supervisor is informed so
    the IRC connection can be torn down once all subscribers leave.
    """
    await websocket.accept()
    await irc_supervisor.acquire(channel_login)
    queue = chat_bus.subscribe(channel_login)
    try:
        while True:
            event = await queue.get()
            await websocket.send_json(event)
    except WebSocketDisconnect:
        return
    finally:
        chat_bus.unsubscribe(channel_login, queue)
        await irc_supervisor.release(channel_login)
