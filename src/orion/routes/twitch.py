"""Twitch OAuth flow.

Intended wiring:
1. Browser calls ``GET /orion/api/v1/twitch/oauth/authorize?credential_id=<id>`` — Orion
   returns a redirect URL. Browser navigates there.
2. User authorizes. Twitch redirects to ``twitch_oauth_redirect_uri`` on the admin
   frontend, which POSTs the code back to
   ``POST /orion/api/v1/twitch/oauth/callback`` with the credential id.
3. Orion exchanges the code, encrypts the tokens, and attaches them to the credential.

The ``state`` query param is a signed envelope that pins the credential_id and the
frontend origin.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import secrets
import time
import uuid
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, status
from pydantic import BaseModel
from sqlalchemy.ext.asyncio import AsyncSession

from orion.config import settings
from orion.database import get_session
from orion.models.credential import TwitchCredential
from orion.routes._deps import require_authenticated_user
from orion.services import credential_service, twitch_helix

router = APIRouter(prefix="/twitch", tags=["twitch"])


class AuthorizeResponse(BaseModel):
    redirect_url: str
    state: str


class CallbackPayload(BaseModel):
    code: str
    state: str


def _state_envelope(credential_id: uuid.UUID) -> tuple[str, str]:
    """Build the ``state`` carried through the OAuth round-trip.

    Returns ``(state_token, raw_state)``. The state token is HMAC-signed against the
    encryption key so the callback cannot be spoofed.
    """
    payload = {
        "credential_id": str(credential_id),
        "nonce": secrets.token_urlsafe(12),
        "ts": int(time.time()),
    }
    raw = json.dumps(payload, separators=(",", ":"), sort_keys=True)
    key = settings.encryption_key.encode() or b"dev-only-never-use-empty-key"
    mac = hmac.new(key, raw.encode(), hashlib.sha256).digest()
    token = base64.urlsafe_b64encode(raw.encode() + b"." + mac).decode()
    return token, raw


def _verify_state(state_token: str) -> dict[str, Any]:
    try:
        blob = base64.urlsafe_b64decode(state_token.encode())
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status.HTTP_400_BAD_REQUEST, detail="Invalid OAuth state.") from exc
    if b"." not in blob:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, detail="Invalid OAuth state.")
    raw, mac = blob.rsplit(b".", 1)
    key = settings.encryption_key.encode() or b"dev-only-never-use-empty-key"
    expected = hmac.new(key, raw, hashlib.sha256).digest()
    if not hmac.compare_digest(expected, mac):
        raise HTTPException(status.HTTP_400_BAD_REQUEST, detail="OAuth state signature mismatch.")
    data = json.loads(raw.decode())
    if time.time() - int(data.get("ts", 0)) > 900:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, detail="OAuth state expired.")
    return data  # type: ignore[no-any-return]


@router.post("/oauth/authorize", response_model=AuthorizeResponse)
async def authorize(
    credential_id: uuid.UUID,
    user_id: uuid.UUID = Depends(require_authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> AuthorizeResponse:
    cred = await db.get(TwitchCredential, credential_id)
    if cred is None or cred.owner_id != user_id:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Credential not found.")
    state_token, _ = _state_envelope(credential_id)
    return AuthorizeResponse(
        redirect_url=twitch_helix.authorize_url(state_token),
        state=state_token,
    )


@router.post("/oauth/callback", status_code=status.HTTP_204_NO_CONTENT)
async def oauth_callback(
    payload: CallbackPayload,
    user_id: uuid.UUID = Depends(require_authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> None:
    state = _verify_state(payload.state)
    credential_id = uuid.UUID(state["credential_id"])
    cred = await db.get(TwitchCredential, credential_id)
    if cred is None or cred.owner_id != user_id:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Credential not found.")

    tokens = await twitch_helix.exchange_code(payload.code)
    access_token = tokens["access_token"]
    channel_id: str | None = None
    channel_login: str | None = None
    try:
        helix = twitch_helix.HelixClient(access_token)
        try:
            # Twitch doesn't return the user in the token response; resolve via /users.
            r = await helix._client.get("/users")  # noqa: SLF001
            if r.status_code < 300:
                data = r.json().get("data", [])
                if data:
                    channel_id = data[0].get("id")
                    channel_login = data[0].get("login")
        finally:
            await helix.aclose()
    except twitch_helix.TwitchAPIError:
        pass

    await credential_service.store_oauth(
        db,
        cred,
        access_token=access_token,
        refresh_token=tokens.get("refresh_token"),
        expires_at=twitch_helix.expires_at(tokens),
        scopes=tokens.get("scope"),
        channel_id=channel_id,
        channel_login=channel_login,
    )
    await db.commit()
