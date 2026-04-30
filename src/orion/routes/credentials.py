"""Twitch credential CRUD. Secrets never leave the server.

The one exception is :func:`get_stream_key` — Pulsar (broadcast engine
bundled in Prism) needs the decrypted Twitch stream key to push RTMP
directly. The endpoint is owner-scoped and auth-required ; trust is
anchored on the desktop client (Prism is JWT-authenticated, IPC-isolated
from the web, and the user already controls the credential).
"""

from __future__ import annotations

import uuid

from fastapi import APIRouter, Depends, HTTPException, Response, status
from pydantic import BaseModel
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.models.credential import TwitchCredential
from orion.routes._deps import authenticated_user
from orion.schemas.credential import CredentialCreate, CredentialRead, CredentialUpdate
from orion.services import credential_service, encryption

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
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> list[dict[str, object]]:
    creds = await credential_service.list_for_owner(db, user_id)
    return [_project(c) for c in creds]


@router.post("", response_model=CredentialRead, status_code=status.HTTP_201_CREATED)
async def create_credential(
    payload: CredentialCreate,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, object]:
    cred = await credential_service.create(db, user_id, payload)
    await db.commit()
    return _project(cred)


@router.put("/{credential_id}", response_model=CredentialRead)
async def update_credential(
    credential_id: uuid.UUID,
    payload: CredentialUpdate,
    user_id: uuid.UUID | None = Depends(authenticated_user),
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
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> Response:
    cred = await db.get(TwitchCredential, credential_id)
    if cred is None or cred.owner_id != user_id:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Credential not found.")
    await db.delete(cred)
    await db.commit()
    return Response(status_code=status.HTTP_204_NO_CONTENT)


class StreamKeyRead(BaseModel):
    """Decrypted Twitch stream key. Returned to the credential owner only.

    Pulsar (broadcast engine bundled in Prism) needs the plaintext key
    to push RTMP directly to ``rtmp://live.twitch.tv/app/<key>``. The
    key is AES-GCM encrypted at rest and only released to the
    authenticated owner over a JWT-protected channel.
    """

    stream_key: str


@router.get("/{credential_id}/stream-key", response_model=StreamKeyRead)
async def get_stream_key(
    credential_id: uuid.UUID,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> StreamKeyRead:
    cred = await db.get(TwitchCredential, credential_id)
    if cred is None or cred.owner_id != user_id:
        raise HTTPException(status_code=status.HTTP_404_NOT_FOUND, detail="Credential not found.")
    try:
        plaintext = encryption.decrypt(
            cred.stream_key_ciphertext, cred.stream_key_nonce
        )
    except Exception as exc:  # noqa: BLE001 — surface decrypt failure to the caller
        raise HTTPException(
            status_code=status.HTTP_422_UNPROCESSABLE_ENTITY,
            detail=(
                f"Stored stream key could not be decrypted ({exc.__class__.__name__}). "
                "The Orion ENCRYPTION_KEY may have rotated since this credential "
                "was saved — re-enter a fresh key from your Twitch dashboard."
            ),
        ) from exc
    return StreamKeyRead(stream_key=plaintext)
