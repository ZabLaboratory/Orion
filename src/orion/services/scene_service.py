"""Scene CRUD helpers."""

from __future__ import annotations

import uuid

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from orion.models.scene import Scene
from orion.schemas.scene import SceneCreate, SceneUpdate


async def create(db: AsyncSession, owner_id: uuid.UUID | None, payload: SceneCreate) -> Scene:
    scene = Scene(
        owner_id=owner_id,
        name=payload.name,
        description=payload.description,
        width=payload.width,
        height=payload.height,
        fps=payload.fps,
        config=payload.config,
        thumbnail=payload.thumbnail,
    )
    db.add(scene)
    await db.flush()
    await db.refresh(scene)
    return scene


async def update(db: AsyncSession, scene: Scene, payload: SceneUpdate) -> Scene:
    if payload.name is not None:
        scene.name = payload.name
    if payload.description is not None:
        scene.description = payload.description
    if payload.width is not None:
        scene.width = payload.width
    if payload.height is not None:
        scene.height = payload.height
    if payload.fps is not None:
        scene.fps = payload.fps
    if payload.config is not None:
        scene.config = payload.config
    if payload.thumbnail is not None:
        scene.thumbnail = payload.thumbnail
    await db.flush()
    await db.refresh(scene)
    return scene


async def list_all(db: AsyncSession, owner_id: uuid.UUID | None) -> list[Scene]:
    q = select(Scene).order_by(Scene.updated_at.desc())
    if owner_id is not None:
        q = q.where(Scene.owner_id == owner_id)
    result = await db.execute(q)
    return list(result.scalars().all())
