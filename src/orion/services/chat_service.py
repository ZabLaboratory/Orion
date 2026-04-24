"""Chat component CRUD + in-process event fan-out for the chat WebSocket."""

from __future__ import annotations

import asyncio
import uuid
from collections import defaultdict

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from orion.models.chat import ChatComponent
from orion.schemas.chat import ChatComponentCreate, ChatComponentUpdate


class ChatEventBus:
    """In-process pub/sub used to fan chat messages out to connected WebSockets.

    Replace with Redis pub/sub if Orion ever scales beyond one replica.
    """

    def __init__(self) -> None:
        self._subscribers: dict[str, list[asyncio.Queue[dict[str, object]]]] = defaultdict(list)

    def subscribe(self, channel: str) -> asyncio.Queue[dict[str, object]]:
        q: asyncio.Queue[dict[str, object]] = asyncio.Queue(maxsize=1024)
        self._subscribers[channel.lower()].append(q)
        return q

    def unsubscribe(self, channel: str, q: asyncio.Queue[dict[str, object]]) -> None:
        lst = self._subscribers.get(channel.lower())
        if lst and q in lst:
            lst.remove(q)

    async def publish(self, channel: str, event: dict[str, object]) -> None:
        for q in list(self._subscribers.get(channel.lower(), [])):
            if q.full():
                # Drop oldest on overflow — chat is lossy by design.
                try:
                    q.get_nowait()
                except asyncio.QueueEmpty:
                    pass
            await q.put(event)


chat_bus = ChatEventBus()


async def create(
    db: AsyncSession, owner_id: uuid.UUID | None, payload: ChatComponentCreate
) -> ChatComponent:
    comp = ChatComponent(
        owner_id=owner_id,
        name=payload.name,
        blueprint_ref=payload.blueprint_ref,
        config=payload.config,
        placement=payload.placement,
        triggers=payload.triggers,
        is_enabled=payload.is_enabled,
    )
    db.add(comp)
    await db.flush()
    await db.refresh(comp)
    return comp


async def update(
    db: AsyncSession, comp: ChatComponent, payload: ChatComponentUpdate
) -> ChatComponent:
    if payload.name is not None:
        comp.name = payload.name
    if payload.blueprint_ref is not None:
        comp.blueprint_ref = payload.blueprint_ref
    if payload.config is not None:
        comp.config = payload.config
    if payload.placement is not None:
        comp.placement = payload.placement
    if payload.triggers is not None:
        comp.triggers = payload.triggers
    if payload.is_enabled is not None:
        comp.is_enabled = payload.is_enabled
    await db.flush()
    await db.refresh(comp)
    return comp


async def list_all(
    db: AsyncSession, owner_id: uuid.UUID | None, *, blueprint_ref: str | None = None
) -> list[ChatComponent]:
    q = select(ChatComponent).order_by(ChatComponent.updated_at.desc())
    if owner_id is not None:
        q = q.where(ChatComponent.owner_id == owner_id)
    if blueprint_ref is not None:
        q = q.where(ChatComponent.blueprint_ref == blueprint_ref)
    result = await db.execute(q)
    return list(result.scalars().all())
