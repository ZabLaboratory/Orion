"""Scene CRUD routes."""

from __future__ import annotations

import uuid

from fastapi import APIRouter, Depends, HTTPException, Query, Response, status
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.models.scene import Scene
from orion.routes._deps import authenticated_user
from orion.schemas.scene import SceneCreate, SceneRead, SceneSummary, SceneUpdate
from orion.services import scene_service

router = APIRouter(prefix="/scenes", tags=["scenes"])


@router.get("", response_model=list[SceneSummary])
async def list_scenes(
    mine: bool = Query(default=False, description="Scope to the authenticated user."),
    db: AsyncSession = Depends(get_session),
    user_id: uuid.UUID | None = Depends(authenticated_user),
) -> list[Scene]:
    owner = user_id if mine and user_id is not None else None
    return await scene_service.list_all(db, owner)


@router.post("", response_model=SceneRead, status_code=status.HTTP_201_CREATED)
async def create_scene(
    payload: SceneCreate,
    db: AsyncSession = Depends(get_session),
    user_id: uuid.UUID | None = Depends(authenticated_user),
) -> Scene:
    scene = await scene_service.create(db, user_id, payload)
    await db.commit()
    return scene


@router.get("/{scene_id}", response_model=SceneRead)
async def get_scene(
    scene_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
) -> Scene:
    scene = await db.get(Scene, scene_id)
    if scene is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Scene not found.")
    return scene


@router.put("/{scene_id}", response_model=SceneRead)
async def update_scene(
    scene_id: uuid.UUID,
    payload: SceneUpdate,
    db: AsyncSession = Depends(get_session),
) -> Scene:
    scene = await db.get(Scene, scene_id)
    if scene is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Scene not found.")
    scene = await scene_service.update(db, scene, payload)
    await db.commit()
    return scene


@router.delete("/{scene_id}", status_code=status.HTTP_204_NO_CONTENT)
async def delete_scene(
    scene_id: uuid.UUID,
    db: AsyncSession = Depends(get_session),
) -> Response:
    scene = await db.get(Scene, scene_id)
    if scene is None:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Scene not found.")
    await db.delete(scene)
    await db.commit()
    return Response(status_code=status.HTTP_204_NO_CONTENT)
