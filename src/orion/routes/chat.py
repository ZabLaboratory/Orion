"""Chat live WebSocket fan-out.

Chat *components* live in ZabCanvas as scene components (Blue blueprints
resolved at render time). Orion only owns the IRC pump (it has the OAuth
tokens) and this WebSocket — Blue's chat-events nodes subscribe here.
"""

from __future__ import annotations

from fastapi import APIRouter, WebSocket, WebSocketDisconnect

from orion.services.chat_service import chat_bus

router = APIRouter(prefix="/chat", tags=["chat"])


@router.websocket("/live/{channel_login}")
async def chat_live_socket(websocket: WebSocket, channel_login: str) -> None:
    """Stream Twitch IRC events for a channel as JSON frames.

    The IRC connection itself is owned by ``ChatSupervisor`` (lifespan task);
    this endpoint is a passive consumer of ``chat_bus``. If no supervisor task
    is currently joined to the channel, the subscriber sees an empty stream
    until one comes online — never an error.
    """
    await websocket.accept()
    queue = chat_bus.subscribe(channel_login)
    try:
        while True:
            event = await queue.get()
            await websocket.send_json(event)
    except WebSocketDisconnect:
        return
    finally:
        chat_bus.unsubscribe(channel_login, queue)
