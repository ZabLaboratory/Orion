"""Chat component CRUD + live message WebSocket."""

from __future__ import annotations

import asyncio
import uuid

from fastapi import APIRouter, Depends, HTTPException, Query, Response, WebSocket, WebSocketDisconnect, status
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import async_session, get_session
from orion.models.chat import ChatComponent
from orion.routes._deps import authenticated_user
from orion.schemas.chat import ChatComponentCreate, ChatComponentRead, ChatComponentUpdate
from orion.services import chat_service

router = APIRouter(prefix="/chat", tags=["chat"])


# ---------------------------------------------------------- components CRUD
@router.get("/components", response_model=list[ChatComponentRead])
async def list_components(
    blueprint_ref: str | None = Query(default=None),
    mine: bool = Query(default=False),
    db: AsyncSession = Depends(get_session),
    user_id: uuid.UUID | None = Depends(authenticated_user),
) -> list[ChatComponent]:
    owner = user_id if mine and user_id is not None else None
    return await chat_service.list_all(db, owner, blueprint_ref=blueprint_ref)


@router.post("/components", response_model=ChatComponentRead, status_code=status.HTTP_201_CREATED)
async def create_component(
    payload: ChatComponentCreate,
    db: AsyncSession = Depends(get_session),
    user_id: uuid.UUID | None = Depends(authenticated_user),
) -> ChatComponent:
    comp = await chat_service.create(db, user_id, payload)
    await db.commit()
    return comp


@router.get("/components/{component_id}", response_model=ChatComponentRead)
async def get_component(
    component_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
) -> ChatComponent:
    comp = await db.get(ChatComponent, component_id)
    if comp is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Chat component not found.")
    return comp


@router.put("/components/{component_id}", response_model=ChatComponentRead)
async def update_component(
    component_id: uuid.UUID,
    payload: ChatComponentUpdate,
    db: AsyncSession = Depends(get_session),
) -> ChatComponent:
    comp = await db.get(ChatComponent, component_id)
    if comp is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Chat component not found.")
    comp = await chat_service.update(db, comp, payload)
    await db.commit()
    return comp


@router.delete("/components/{component_id}", status_code=status.HTTP_204_NO_CONTENT)
async def delete_component(
    component_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
) -> Response:
    comp = await db.get(ChatComponent, component_id)
    if comp is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Chat component not found.")
    await db.delete(comp)
    await db.commit()
    return Response(status_code=status.HTTP_204_NO_CONTENT)


# ---------------------------------------------------------- live events WS
@router.websocket("/live/{channel_login}")
async def chat_live_socket(websocket: WebSocket, channel_login: str) -> None:
    """Streams chat events for a Twitch channel to subscribed clients.

    The IRC connection itself is owned by the ``stream_manager`` runner — this WS
    simply consumes from ``chat_bus`` and forwards each event as JSON. If no IRC is
    running for that channel, the subscriber simply sees an empty stream.
    """
    await websocket.accept()
    queue = chat_service.chat_bus.subscribe(channel_login)
    try:
        while True:
            event = await queue.get()
            await websocket.send_json(event)
    except WebSocketDisconnect:
        return
    finally:
        chat_service.chat_bus.unsubscribe(channel_login, queue)


async def _irc_runner_placeholder() -> None:
    """Placeholder for the background task that owns the Twitch IRC connection.

    Wired by the deployer/operator once a credential opts into chat. Kept here as a
    docstring anchor until the full runtime is lifted in with the blueprint engine.
    """
    await asyncio.sleep(0)
    async with async_session() as _session:
        # Future: iterate credentials with oauth + chat scope, spawn TwitchChatClient
        # per active stream, push decoded messages into chat_bus.publish(...).
        _ = _session
