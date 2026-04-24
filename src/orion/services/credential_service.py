"""CRUD + encryption glue for Twitch credentials."""

from __future__ import annotations

import uuid
from datetime import datetime

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from orion.models.credential import TwitchCredential
from orion.schemas.credential import CredentialCreate, CredentialUpdate
from orion.services import encryption


async def create(
    db: AsyncSession, owner_id: uuid.UUID, payload: CredentialCreate
) -> TwitchCredential:
    sk_ct, sk_nonce = encryption.encrypt(payload.stream_key)
    cred = TwitchCredential(
        owner_id=owner_id,
        label=payload.label,
        channel_login=payload.channel_login,
        channel_id=payload.channel_id,
        stream_key_ciphertext=sk_ct,
        stream_key_nonce=sk_nonce,
    )
    db.add(cred)
    await db.flush()
    await db.refresh(cred)
    return cred


async def update(
    db: AsyncSession, cred: TwitchCredential, payload: CredentialUpdate
) -> TwitchCredential:
    if payload.label is not None:
        cred.label = payload.label
    if payload.channel_login is not None:
        cred.channel_login = payload.channel_login
    if payload.channel_id is not None:
        cred.channel_id = payload.channel_id
    if payload.stream_key is not None:
        sk_ct, sk_nonce = encryption.encrypt(payload.stream_key)
        cred.stream_key_ciphertext = sk_ct
        cred.stream_key_nonce = sk_nonce
    await db.flush()
    await db.refresh(cred)
    return cred


async def list_for_owner(db: AsyncSession, owner_id: uuid.UUID) -> list[TwitchCredential]:
    q = select(TwitchCredential).where(TwitchCredential.owner_id == owner_id).order_by(
        TwitchCredential.created_at.desc()
    )
    result = await db.execute(q)
    return list(result.scalars().all())


async def store_oauth(
    db: AsyncSession,
    cred: TwitchCredential,
    *,
    access_token: str,
    refresh_token: str | None,
    expires_at: datetime | None,
    scopes: list[str] | None,
    channel_id: str | None,
    channel_login: str | None,
) -> TwitchCredential:
    access_ct, access_nonce = encryption.encrypt(access_token)
    cred.oauth_access_ciphertext = access_ct
    cred.oauth_access_nonce = access_nonce
    if refresh_token:
        refresh_ct, refresh_nonce = encryption.encrypt(refresh_token)
        cred.oauth_refresh_ciphertext = refresh_ct
        cred.oauth_refresh_nonce = refresh_nonce
    cred.oauth_expires_at = expires_at
    cred.oauth_scopes = scopes
    if channel_id:
        cred.channel_id = channel_id
    if channel_login:
        cred.channel_login = channel_login
    await db.flush()
    await db.refresh(cred)
    return cred


def read_oauth_access(cred: TwitchCredential) -> str | None:
    if cred.oauth_access_ciphertext is None or cred.oauth_access_nonce is None:
        return None
    return encryption.decrypt(cred.oauth_access_ciphertext, cred.oauth_access_nonce)


def read_oauth_refresh(cred: TwitchCredential) -> str | None:
    if cred.oauth_refresh_ciphertext is None or cred.oauth_refresh_nonce is None:
        return None
    return encryption.decrypt(cred.oauth_refresh_ciphertext, cred.oauth_refresh_nonce)
