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
from fastapi.responses import HTMLResponse
from pydantic import BaseModel
from sqlalchemy.ext.asyncio import AsyncSession

from orion.config import settings
from orion.database import get_session
from orion.models.credential import TwitchCredential
from orion.routes._deps import authenticated_user
from orion.services import credential_service, helix_session, twitch_helix

router = APIRouter(prefix="/twitch", tags=["twitch"])


# ── Helix surface ─────────────────────────────────────────────────────────


class ChannelUpdate(BaseModel):
    """Subset of `/helix/channels` PATCH the operator typically tweaks
    before going live. ``broadcaster_language`` follows ISO 639-1."""

    title: str | None = None
    game_id: str | None = None
    tags: list[str] | None = None
    broadcaster_language: str | None = None


async def _load_owned_credential(
    db: AsyncSession,
    credential_id: uuid.UUID,
    user_id: uuid.UUID | None,
) -> TwitchCredential:
    cred = await db.get(TwitchCredential, credential_id)
    if cred is None or cred.owner_id != user_id:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Credential not found.")
    return cred


def _map_helix_errors(exc: Exception) -> HTTPException:
    if isinstance(exc, helix_session.CredentialMissingOAuth):
        return HTTPException(
            status.HTTP_412_PRECONDITION_FAILED,
            detail="Credential has no OAuth tokens — run the OAuth flow first.",
        )
    if isinstance(exc, helix_session.CredentialMissingChannelId):
        return HTTPException(
            status.HTTP_412_PRECONDITION_FAILED,
            detail="Credential has no resolved channel_id — re-run the OAuth flow.",
        )
    if isinstance(exc, twitch_helix.TwitchAuthError):
        return HTTPException(
            status.HTTP_401_UNAUTHORIZED,
            detail="Twitch rejected the access token. Re-authorize the credential.",
        )
    if isinstance(exc, twitch_helix.TwitchAPIError):
        return HTTPException(
            status.HTTP_502_BAD_GATEWAY,
            detail=f"Twitch API error: {exc}",
        )
    raise exc  # caller didn't expect this — let FastAPI 500 it


@router.get("/credentials/{credential_id}/channel")
async def get_channel(
    credential_id: uuid.UUID,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, Any]:
    """Channel info for the credential's broadcaster (title, category, tags)."""
    cred = await _load_owned_credential(db, credential_id, user_id)
    try:
        broadcaster_id = helix_session.require_channel_id(cred)
        result = await helix_session.call_with_credential(
            db, cred, lambda h: h.get_channel(broadcaster_id)
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc
    if result is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Twitch returned no channel data.")
    return result


@router.patch("/credentials/{credential_id}/channel", status_code=status.HTTP_204_NO_CONTENT)
async def patch_channel(
    credential_id: uuid.UUID,
    payload: ChannelUpdate,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> None:
    """Update title / category / language / tags on the broadcaster.

    Requires the credential to carry an OAuth token with the
    ``channel:manage:broadcast`` scope.
    """
    cred = await _load_owned_credential(db, credential_id, user_id)
    try:
        broadcaster_id = helix_session.require_channel_id(cred)
        await helix_session.call_with_credential(
            db,
            cred,
            lambda h: h.update_channel(
                broadcaster_id,
                title=payload.title,
                game_id=payload.game_id,
                tags=payload.tags,
                broadcaster_language=payload.broadcaster_language,
            ),
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc


@router.get("/credentials/{credential_id}/schedule")
async def get_schedule(
    credential_id: uuid.UUID,
    first: int = 25,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, Any]:
    cred = await _load_owned_credential(db, credential_id, user_id)
    try:
        broadcaster_id = helix_session.require_channel_id(cred)
        return await helix_session.call_with_credential(
            db, cred, lambda h: h.get_schedule(broadcaster_id, first=first)
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc


@router.get("/credentials/{credential_id}/clips")
async def get_clips(
    credential_id: uuid.UUID,
    first: int = 20,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, Any]:
    cred = await _load_owned_credential(db, credential_id, user_id)
    try:
        broadcaster_id = helix_session.require_channel_id(cred)
        return await helix_session.call_with_credential(
            db, cred, lambda h: h.get_clips(broadcaster_id, first=first)
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc


@router.get("/credentials/{credential_id}/predictions")
async def get_predictions(
    credential_id: uuid.UUID,
    first: int = 25,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, Any]:
    cred = await _load_owned_credential(db, credential_id, user_id)
    try:
        broadcaster_id = helix_session.require_channel_id(cred)
        return await helix_session.call_with_credential(
            db, cred, lambda h: h.get_predictions(broadcaster_id, first=first)
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc


@router.get("/credentials/{credential_id}/stream")
async def get_stream(
    credential_id: uuid.UUID,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, Any]:
    """Live-stream snapshot for the credential's broadcaster.

    Returns the raw Helix ``streams`` payload when live, or
    ``{"live": false}`` when offline — saves the dashboard from
    branching on a 404 vs empty array.
    """
    cred = await _load_owned_credential(db, credential_id, user_id)
    try:
        broadcaster_id = helix_session.require_channel_id(cred)
        result = await helix_session.call_with_credential(
            db, cred, lambda h: h.get_stream_by_user_id(broadcaster_id)
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc
    if result is None:
        return {"live": False}
    return {"live": True, **result}


@router.get("/credentials/{credential_id}/followers")
async def get_followers(
    credential_id: uuid.UUID,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, int]:
    """Total follower count for the credential's broadcaster."""
    cred = await _load_owned_credential(db, credential_id, user_id)
    try:
        broadcaster_id = helix_session.require_channel_id(cred)
        total = await helix_session.call_with_credential(
            db, cred, lambda h: h.get_followers_total(broadcaster_id)
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc
    return {"total": int(total)}


@router.get("/credentials/{credential_id}/subscribers")
async def get_subscribers(
    credential_id: uuid.UUID,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> dict[str, int]:
    """Subscriber total + tier breakdown.

    Twitch caps the page at 100 — the tier breakdown is an
    approximation when a channel has > 100 subs (the totals from
    Helix are still authoritative). Good enough for a dashboard
    glance ; an exact tier ledger would require pagination.
    """
    cred = await _load_owned_credential(db, credential_id, user_id)
    try:
        broadcaster_id = helix_session.require_channel_id(cred)
        return await helix_session.call_with_credential(
            db, cred, lambda h: h.get_subscribers_total(broadcaster_id)
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc


class AuthorizeResponse(BaseModel):
    redirect_url: str
    state: str


class CallbackPayload(BaseModel):
    code: str
    state: str


def _state_envelope(credential_id: uuid.UUID, redirect_uri: str) -> tuple[str, str]:
    """Build the ``state`` carried through the OAuth round-trip.

    The redirect_uri is signed into the state so the callback can reuse the
    exact same URI during the token exchange — Twitch (and RFC 6749 §4.1.3)
    require redirect_uri to match byte-for-byte between authorize and exchange.

    Returns ``(state_token, raw_state)``. The state token is HMAC-signed against
    the encryption key so the callback cannot be spoofed.
    """
    payload = {
        "credential_id": str(credential_id),
        "redirect_uri": redirect_uri,
        "nonce": secrets.token_urlsafe(12),
        "ts": int(time.time()),
    }
    raw = json.dumps(payload, separators=(",", ":"), sort_keys=True)
    key = settings.encryption_key.encode() or b"dev-only-never-use-empty-key"
    mac = hmac.new(key, raw.encode(), hashlib.sha256).digest()
    token = base64.urlsafe_b64encode(raw.encode() + b"." + mac).decode()
    return token, raw


_SELF_CALLBACK_PATH = "/orion/api/v1/twitch/oauth/callback"


def _is_allowed_redirect_uri(uri: str) -> bool:
    """Decide whether a redirect_uri the renderer hands us is safe to
    sign into the OAuth ``state`` envelope.

    Accept :
      * the configured web URI (legacy frontend flow) ;
      * any HTTPS URL whose path is our own
        ``/orion/api/v1/twitch/oauth/callback`` — Twitch only allows
        HTTPS now, so desktop callers point straight at the gateway,
        which routes back into this very service. The signed state
        is the trust anchor, not the host ;
      * http loopback (kept for legacy desktop flows even though
        Twitch may refuse it at the dev console) ;
      * custom protocol schemes (prism://, streamlabs://) for OS
        deep-link callbacks."""
    if uri == settings.twitch_oauth_redirect_uri:
        return True
    from urllib.parse import urlparse

    parsed = urlparse(uri)
    if parsed.scheme == "https" and parsed.path.endswith(_SELF_CALLBACK_PATH):
        return True
    if parsed.scheme == "http" and parsed.hostname in ("localhost", "127.0.0.1"):
        return True
    if parsed.scheme and parsed.scheme not in {"http", "https", "file", "javascript"}:
        return True
    return False


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
    redirect_uri: str | None = None,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> AuthorizeResponse:
    cred = await db.get(TwitchCredential, credential_id)
    if cred is None or cred.owner_id != user_id:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Credential not found.")

    effective_redirect = redirect_uri or settings.twitch_oauth_redirect_uri
    if not _is_allowed_redirect_uri(effective_redirect):
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST,
            detail=("redirect_uri not allowed. Use the configured web URI or an http://localhost:* loopback."),
        )

    state_token, _ = _state_envelope(credential_id, effective_redirect)
    return AuthorizeResponse(
        redirect_url=twitch_helix.authorize_url(state_token, redirect_uri=effective_redirect),
        state=state_token,
    )


async def _exchange_and_store(
    db: AsyncSession,
    code: str,
    state_token: str,
    caller_user_id: uuid.UUID | None,
    *,
    enforce_owner: bool,
) -> None:
    """Verify state envelope, exchange the OAuth code, persist tokens.

    ``enforce_owner=True`` for the POST flow (renderer is authenticated)
    ; False for the browser GET flow (the signed state is the only
    trust anchor — the browser doesn't carry a Zablab session)."""
    state = _verify_state(state_token)
    credential_id = uuid.UUID(state["credential_id"])
    cred = await db.get(TwitchCredential, credential_id)
    if cred is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Credential not found.")
    if enforce_owner and cred.owner_id != caller_user_id:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Credential not found.")

    redirect_uri = state.get("redirect_uri") or settings.twitch_oauth_redirect_uri
    tokens = await twitch_helix.exchange_code(code, redirect_uri=redirect_uri)
    access_token = tokens["access_token"]
    channel_id: str | None = None
    channel_login: str | None = None
    try:
        helix = twitch_helix.HelixClient(access_token)
        try:
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


@router.post("/oauth/callback", status_code=status.HTTP_204_NO_CONTENT)
async def oauth_callback(
    payload: CallbackPayload,
    user_id: uuid.UUID | None = Depends(authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> None:
    """POST flow — kept for backwards compat with the legacy web
    relay. Renderer is authenticated, ownership is enforced."""
    await _exchange_and_store(
        db, payload.code, payload.state, user_id, enforce_owner=True
    )


_CALLBACK_HTML_OK = """<!doctype html>
<html lang="en"><head><meta charset="utf-8" />
<title>Twitch connected</title>
<style>
  html,body{margin:0;height:100%;background:#0d0d11;color:#e7e7ea;
    font-family:ui-sans-serif,system-ui,sans-serif;
    display:flex;align-items:center;justify-content:center;}
  .card{border:1px solid #26262d;background:#1c1c21;padding:32px 40px;max-width:420px;}
  h1{font-size:18px;margin:0 0 8px;color:#f19b41;letter-spacing:.02em;}
  p{font-size:13px;color:#b4b4b9;line-height:1.55;margin:0;}
</style></head>
<body><div class="card">
  <h1>Twitch connected</h1>
  <p>You can close this tab and return to Prism — the readiness modal will pick up the new credential within a few seconds.</p>
</div></body></html>"""


def _callback_error_html(detail: str) -> str:
    safe = detail.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    return (
        '<!doctype html><html lang="en"><head><meta charset="utf-8" />'
        "<title>Twitch connection failed</title>"
        "<style>html,body{margin:0;height:100%;background:#0d0d11;color:#e7e7ea;"
        "font-family:ui-sans-serif,system-ui,sans-serif;"
        "display:flex;align-items:center;justify-content:center;}"
        ".card{border:1px solid #26262d;background:#1c1c21;padding:32px 40px;max-width:520px;}"
        "h1{font-size:18px;margin:0 0 8px;color:#f25555;letter-spacing:.02em;}"
        "p{font-size:13px;color:#b4b4b9;line-height:1.55;margin:0 0 6px;}"
        "code{background:#141419;padding:2px 6px;border:1px solid #26262d;font-size:11px;}"
        f'</style></head><body><div class="card"><h1>Twitch connection failed</h1>'
        "<p>Prism could not finish the OAuth exchange.</p>"
        f"<p><code>{safe}</code></p></div></body></html>"
    )


@router.get("/oauth/callback", response_class=HTMLResponse)
async def oauth_callback_redirect(
    code: str | None = None,
    state: str | None = None,
    error: str | None = None,
    error_description: str | None = None,
    db: AsyncSession = Depends(get_session),
) -> HTMLResponse:
    """GET flow — Twitch redirects the browser straight to this URL
    after the user approves consent. The browser doesn't carry a
    Zablab JWT (it's not the Prism IPC channel) ; trust is anchored
    on the HMAC-signed ``state`` envelope. Prism detects completion
    by polling ``GET /credentials`` and watching ``has_oauth`` flip."""
    if error:
        return HTMLResponse(
            _callback_error_html(error_description or error),
            status_code=400,
        )
    if not code or not state:
        return HTMLResponse(
            _callback_error_html("Missing code or state in callback URL."),
            status_code=400,
        )
    try:
        await _exchange_and_store(db, code, state, None, enforce_owner=False)
    except HTTPException as exc:
        detail = exc.detail if isinstance(exc.detail, str) else str(exc.detail)
        return HTMLResponse(_callback_error_html(detail), status_code=exc.status_code)
    except Exception as exc:  # noqa: BLE001 — surface anything else to the browser
        return HTMLResponse(
            _callback_error_html(f"{exc.__class__.__name__}: {exc}"), status_code=500
        )
    return HTMLResponse(_CALLBACK_HTML_OK, status_code=200)
