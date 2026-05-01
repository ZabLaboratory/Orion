"""Twitch Helix + OAuth integration.

Only the bits Orion needs:
- Authorization Code Grant — user authorizes, we exchange code for tokens
- ``/oauth2/token`` refresh
- ``/helix/channels`` GET/PATCH — read/update title, game, tags
- ``/helix/streams`` GET by ``user_id`` — detect whether the stream is actually live
  on Twitch (independent verification of the RTMP push)
- ``/helix/users`` — resolve login → user_id
"""

from __future__ import annotations

import logging
from datetime import UTC, datetime, timedelta
from typing import Any

import httpx

from orion.config import settings

logger = logging.getLogger(__name__)

_AUTH_BASE = "https://id.twitch.tv"
_API_BASE = "https://api.twitch.tv/helix"

REQUIRED_SCOPES = (
    # Channel ops surfaced by the Helix expansion.
    "channel:manage:broadcast",
    "channel:manage:schedule",
    "channel:read:predictions",
    "channel:manage:predictions",
    "channel:read:subscriptions",
    # Live-stats Dashboard surface.
    "moderator:read:followers",
    # IRC chat — covers both the read pump and the (future) outbound
    # send path the chat WS exposes.
    "chat:read",
    "chat:edit",
    # Identity — needed at OAuth callback to resolve channel_id /
    # channel_login via /users.
    "user:read:email",
)


class TwitchAPIError(RuntimeError):
    """Non-2xx response from the Twitch API."""


class TwitchAuthError(TwitchAPIError):
    """The access token is rejected (401). Caller should refresh and retry."""


def authorize_url(
    state: str,
    scopes: tuple[str, ...] = REQUIRED_SCOPES,
    redirect_uri: str | None = None,
) -> str:
    """Build the authorization redirect URL for the user."""
    if not settings.twitch_client_id:
        raise TwitchAPIError("TWITCH_CLIENT_ID not configured.")
    params = {
        "client_id": settings.twitch_client_id,
        "redirect_uri": redirect_uri or settings.twitch_oauth_redirect_uri,
        "response_type": "code",
        "scope": " ".join(scopes),
        "state": state,
        "force_verify": "true",
    }
    from urllib.parse import urlencode

    return f"{_AUTH_BASE}/oauth2/authorize?{urlencode(params)}"


async def exchange_code(code: str, redirect_uri: str | None = None) -> dict[str, Any]:
    """Exchange an OAuth ``code`` for tokens. Returns the raw Twitch payload.

    ``redirect_uri`` MUST be identical to the one used during the authorize
    step — Twitch (per RFC 6749 §4.1.3) rejects the exchange otherwise.
    """
    if not (settings.twitch_client_id and settings.twitch_client_secret):
        raise TwitchAPIError("Twitch client credentials not configured.")

    async with httpx.AsyncClient(timeout=10.0) as client:
        r = await client.post(
            f"{_AUTH_BASE}/oauth2/token",
            data={
                "client_id": settings.twitch_client_id,
                "client_secret": settings.twitch_client_secret,
                "code": code,
                "grant_type": "authorization_code",
                "redirect_uri": redirect_uri or settings.twitch_oauth_redirect_uri,
            },
        )
    if r.status_code >= 300:
        raise TwitchAPIError(f"exchange_code -> {r.status_code}: {r.text}")
    return r.json()  # type: ignore[no-any-return]


async def refresh_token(refresh: str) -> dict[str, Any]:
    async with httpx.AsyncClient(timeout=10.0) as client:
        r = await client.post(
            f"{_AUTH_BASE}/oauth2/token",
            data={
                "client_id": settings.twitch_client_id,
                "client_secret": settings.twitch_client_secret,
                "refresh_token": refresh,
                "grant_type": "refresh_token",
            },
        )
    if r.status_code >= 300:
        raise TwitchAPIError(f"refresh_token -> {r.status_code}: {r.text}")
    return r.json()  # type: ignore[no-any-return]


def expires_at(payload: dict[str, Any]) -> datetime | None:
    """Absolute expiry from a Twitch token payload."""
    if "expires_in" not in payload:
        return None
    return datetime.now(tz=UTC) + timedelta(seconds=int(payload["expires_in"]))


class HelixClient:
    """Per-user Helix client. Construct with a valid access_token."""

    def __init__(self, access_token: str) -> None:
        if not settings.twitch_client_id:
            raise TwitchAPIError("TWITCH_CLIENT_ID not configured.")
        self._client = httpx.AsyncClient(
            base_url=_API_BASE,
            timeout=10.0,
            headers={
                "Authorization": f"Bearer {access_token}",
                "Client-Id": settings.twitch_client_id,
            },
        )

    async def aclose(self) -> None:
        await self._client.aclose()

    async def get_user_by_login(self, login: str) -> dict[str, Any] | None:
        r = await self._client.get("/users", params={"login": login})
        if r.status_code >= 300:
            raise TwitchAPIError(f"get_user_by_login -> {r.status_code}: {r.text}")
        data = r.json().get("data", [])
        return data[0] if data else None

    async def get_stream_by_user_id(self, user_id: str) -> dict[str, Any] | None:
        r = await self._client.get("/streams", params={"user_id": user_id})
        if r.status_code == 401:
            raise TwitchAuthError("/streams GET 401")
        if r.status_code >= 300:
            raise TwitchAPIError(f"get_stream_by_user_id -> {r.status_code}: {r.text}")
        data = r.json().get("data", [])
        return data[0] if data else None

    async def get_followers_total(self, broadcaster_id: str) -> int:
        """Number of followers on the broadcaster. Requires the
        ``moderator:read:followers`` scope and the auth'd user must be
        the broadcaster (or a moderator of the channel)."""
        r = await self._client.get(
            "/channels/followers",
            params={"broadcaster_id": broadcaster_id, "first": 1},
        )
        if r.status_code == 401:
            raise TwitchAuthError("/channels/followers GET 401")
        if r.status_code >= 300:
            raise TwitchAPIError(
                f"get_followers_total -> {r.status_code}: {r.text}"
            )
        return int(r.json().get("total", 0))

    async def get_subscribers_total(self, broadcaster_id: str) -> dict[str, Any]:
        """Total + tier breakdown for the broadcaster's subscribers.
        Requires ``channel:read:subscriptions``. Returns
        ``{total, points, tier_1, tier_2, tier_3}`` so the dashboard
        can show both raw count and the points-weighted equivalent."""
        r = await self._client.get(
            "/subscriptions",
            params={"broadcaster_id": broadcaster_id, "first": 100},
        )
        if r.status_code == 401:
            raise TwitchAuthError("/subscriptions GET 401")
        if r.status_code >= 300:
            raise TwitchAPIError(
                f"get_subscribers_total -> {r.status_code}: {r.text}"
            )
        body = r.json()
        data = body.get("data", []) or []
        total = int(body.get("total", 0))
        points = int(body.get("points", 0))
        tier_1 = sum(1 for s in data if s.get("tier") == "1000")
        tier_2 = sum(1 for s in data if s.get("tier") == "2000")
        tier_3 = sum(1 for s in data if s.get("tier") == "3000")
        return {
            "total": total,
            "points": points,
            "tier_1": tier_1,
            "tier_2": tier_2,
            "tier_3": tier_3,
        }

    async def update_channel(
        self,
        broadcaster_id: str,
        *,
        title: str | None = None,
        game_id: str | None = None,
        tags: list[str] | None = None,
        broadcaster_language: str | None = None,
    ) -> None:
        payload: dict[str, Any] = {}
        if title is not None:
            payload["title"] = title
        if game_id is not None:
            payload["game_id"] = game_id
        if tags is not None:
            payload["tags"] = tags
        if broadcaster_language is not None:
            payload["broadcaster_language"] = broadcaster_language
        if not payload:
            return
        r = await self._client.patch(
            "/channels",
            params={"broadcaster_id": broadcaster_id},
            json=payload,
        )
        if r.status_code >= 300:
            raise TwitchAPIError(f"update_channel -> {r.status_code}: {r.text}")

    async def get_channel(self, broadcaster_id: str) -> dict[str, Any] | None:
        r = await self._client.get("/channels", params={"broadcaster_id": broadcaster_id})
        if r.status_code == 401:
            raise TwitchAuthError("/channels GET 401")
        if r.status_code >= 300:
            raise TwitchAPIError(f"get_channel -> {r.status_code}: {r.text}")
        data = r.json().get("data", [])
        return data[0] if data else None

    async def get_schedule(
        self,
        broadcaster_id: str,
        *,
        first: int = 25,
    ) -> dict[str, Any]:
        r = await self._client.get(
            "/schedule",
            params={"broadcaster_id": broadcaster_id, "first": first},
        )
        if r.status_code == 401:
            raise TwitchAuthError("/schedule GET 401")
        # 404 = broadcaster has no schedule yet ; surface as empty rather than raising.
        if r.status_code == 404:
            return {"data": {"segments": [], "broadcaster_id": broadcaster_id}, "pagination": {}}
        if r.status_code >= 300:
            raise TwitchAPIError(f"get_schedule -> {r.status_code}: {r.text}")
        return r.json()  # type: ignore[no-any-return]

    async def get_clips(
        self,
        broadcaster_id: str,
        *,
        first: int = 20,
    ) -> dict[str, Any]:
        r = await self._client.get(
            "/clips",
            params={"broadcaster_id": broadcaster_id, "first": first},
        )
        if r.status_code == 401:
            raise TwitchAuthError("/clips GET 401")
        if r.status_code >= 300:
            raise TwitchAPIError(f"get_clips -> {r.status_code}: {r.text}")
        return r.json()  # type: ignore[no-any-return]

    async def get_predictions(
        self,
        broadcaster_id: str,
        *,
        first: int = 25,
    ) -> dict[str, Any]:
        r = await self._client.get(
            "/predictions",
            params={"broadcaster_id": broadcaster_id, "first": first},
        )
        if r.status_code == 401:
            raise TwitchAuthError("/predictions GET 401")
        if r.status_code >= 300:
            raise TwitchAPIError(f"get_predictions -> {r.status_code}: {r.text}")
        return r.json()  # type: ignore[no-any-return]

    # ── EventSub subscription management ────────────────────────────

    async def create_eventsub_subscription(
        self,
        *,
        event_type: str,
        version: str,
        condition: dict[str, Any],
        session_id: str,
    ) -> dict[str, Any]:
        """POST /eventsub/subscriptions for the WebSocket transport.

        Returns the Twitch payload, which carries the assigned
        ``id`` / ``status`` / ``cost`` we mirror in our own row.
        """
        payload = {
            "type": event_type,
            "version": version,
            "condition": condition,
            "transport": {"method": "websocket", "session_id": session_id},
        }
        r = await self._client.post("/eventsub/subscriptions", json=payload)
        if r.status_code == 401:
            raise TwitchAuthError("/eventsub/subscriptions POST 401")
        if r.status_code >= 300:
            raise TwitchAPIError(f"create_eventsub_subscription -> {r.status_code}: {r.text}")
        body = r.json()
        data = body.get("data", [])
        if not data:
            raise TwitchAPIError(f"create_eventsub_subscription: empty data in {body!r}")
        return data[0]  # type: ignore[no-any-return]

    async def list_eventsub_subscriptions(
        self,
        *,
        status: str | None = None,
        event_type: str | None = None,
    ) -> dict[str, Any]:
        params: dict[str, Any] = {}
        if status is not None:
            params["status"] = status
        if event_type is not None:
            params["type"] = event_type
        r = await self._client.get("/eventsub/subscriptions", params=params)
        if r.status_code == 401:
            raise TwitchAuthError("/eventsub/subscriptions GET 401")
        if r.status_code >= 300:
            raise TwitchAPIError(f"list_eventsub_subscriptions -> {r.status_code}: {r.text}")
        return r.json()  # type: ignore[no-any-return]

    async def delete_eventsub_subscription(self, subscription_id: str) -> None:
        r = await self._client.delete(
            "/eventsub/subscriptions", params={"id": subscription_id}
        )
        if r.status_code == 401:
            raise TwitchAuthError("/eventsub/subscriptions DELETE 401")
        if r.status_code == 404:
            return  # already gone — idempotent
        if r.status_code >= 300:
            raise TwitchAPIError(f"delete_eventsub_subscription -> {r.status_code}: {r.text}")
