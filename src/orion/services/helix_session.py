"""Per-credential Helix session helper.

Wraps :class:`twitch_helix.HelixClient` with the lifecycle a route
handler needs : decrypt the access token from the credential, run the
Helix call, transparently refresh + retry once on 401, persist the
refreshed tokens back to the credential.

If refresh is impossible (no refresh token, or Twitch rejects it),
the original :class:`TwitchAuthError` propagates and the route maps
it to a 401.
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from typing import TypeVar

from sqlalchemy.ext.asyncio import AsyncSession

from orion.models.credential import TwitchCredential
from orion.services import credential_service, twitch_helix

logger = logging.getLogger(__name__)

T = TypeVar("T")


class CredentialMissingOAuth(RuntimeError):
    """Credential has no stored OAuth access token. Surface as 412 to the
    caller so the UI can prompt for the OAuth flow."""


class CredentialMissingChannelId(RuntimeError):
    """Credential has no resolved ``channel_id``. Most Helix endpoints
    take a ``broadcaster_id`` ; without it we can't call them. The
    OAuth callback resolves this on first connect — re-running it
    fixes the gap."""


async def _refresh_credential(
    db: AsyncSession,
    cred: TwitchCredential,
) -> str | None:
    """Refresh the OAuth tokens on ``cred`` and return the new access token.

    Returns ``None`` if no refresh token is stored or Twitch rejects
    the refresh.
    """
    refresh = credential_service.read_oauth_refresh(cred)
    if refresh is None:
        return None
    try:
        tokens = await twitch_helix.refresh_token(refresh)
    except twitch_helix.TwitchAPIError:
        logger.exception("twitch refresh_token rejected for credential %s", cred.id)
        return None
    new_access: str = tokens["access_token"]
    await credential_service.store_oauth(
        db,
        cred,
        access_token=new_access,
        refresh_token=tokens.get("refresh_token"),
        expires_at=twitch_helix.expires_at(tokens),
        scopes=tokens.get("scope"),
        channel_id=cred.channel_id,
        channel_login=cred.channel_login,
    )
    await db.commit()
    return new_access


async def call_with_credential(
    db: AsyncSession,
    cred: TwitchCredential,
    op: Callable[[twitch_helix.HelixClient], Awaitable[T]],
) -> T:
    """Run ``op`` against a Helix client built from ``cred``. Retries
    once on :class:`twitch_helix.TwitchAuthError` after refreshing the
    access token. Always closes the underlying httpx client."""
    access = credential_service.read_oauth_access(cred)
    if access is None:
        raise CredentialMissingOAuth("Credential has no stored OAuth access token.")

    client = twitch_helix.HelixClient(access)
    try:
        try:
            return await op(client)
        except twitch_helix.TwitchAuthError:
            await client.aclose()
            new_access = await _refresh_credential(db, cred)
            if new_access is None:
                raise
            client = twitch_helix.HelixClient(new_access)
            return await op(client)
    finally:
        await client.aclose()


def require_channel_id(cred: TwitchCredential) -> str:
    if not cred.channel_id:
        raise CredentialMissingChannelId(
            "Credential has no resolved channel_id ; re-run the OAuth flow."
        )
    return cred.channel_id
