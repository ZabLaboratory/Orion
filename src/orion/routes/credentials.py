"""Twitch credential CRUD. Secrets never leave the server."""

from __future__ import annotations

import uuid

from fastapi import APIRouter, Depends, HTTPException, Response, status
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.models.credential import TwitchCredential
from orion.routes._deps import require_authenticated_user
from orion.schemas.credential import CredentialCreate, CredentialRead, CredentialUpdate
from orion.services import credential_service

router = APIRouter(prefix="/credentials", tags=["credentials"])


def _project(cred: TwitchCredential) -> dict[str, object]:
    return {
        "id": cred.id,
        "owner_id": cred.owner_id,
        "label": cred.label,
        "channel_login": cred.channel_login,
        "channel_id": cred.channel_id,
        "has_oauth": cred.oauth_access_ciphertext is not None,
        "oauth_expires_at": cred.oauth_expires_at,
        "oauth_scopes": cred.oauth_scopes,
        "created_at": cred.created_at,
        "updated_at": cred.updated_at,
    }


@router.get("", response_model=list[CredentialRead])
async def list_credentials(
    user_id: uuid.UUID = Depends(require_authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> list[dict[str, object]]:
    creds = await credential_service.list_for_owner(db, user_id)
    return [_project(c) for c in creds]


@router.post("", response_model=CredentialRead, status_code=status.HTTP_201_CREATED)
async def create_credential(
    payload: CredentialCreate,
    user_id: uuid.UUID = Depends(require_authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, object]:
    cred = await credential_service.create(db, user_id, payload)
    await db.commit()
    return _project(cred)


@router.put("/{credential_id}", response_model=CredentialRead)
async def update_credential(
    credential_id: uuid.UUID,
    payload: CredentialUpdate,
    user_id: uuid.UUID = Depends(require_authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, object]:
    cred = await db.get(TwitchCredential, credential_id)
    if cred is None or cred.owner_id != user_id:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Credential not found.")
    cred = await credential_service.update(db, cred, payload)
    await db.commit()
    return _project(cred)


@router.delete("/{credential_id}", status_code=status.HTTP_204_NO_CONTENT)
async def delete_credential(
    credential_id: uuid.UUID,
    user_id: uuid.UUID = Depends(require_authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> Response:
    cred = await db.get(TwitchCredential, credential_id)
    if cred is None or cred.owner_id != user_id:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Credential not found.")
    await db.delete(cred)
    await db.commit()
    return Response(status_code=status.HTTP_204_NO_CONTENT)
