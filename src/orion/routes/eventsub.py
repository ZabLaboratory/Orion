"""EventSub routes — subscription CRUD + live fan-out WS.

Subscriptions are scoped to a credential : the operator picks which
of their Twitch identities the subscription is attached to. The local
``eventsub_subscriptions`` row mirrors what Twitch returns so the UI
can render the active fleet without re-querying Helix every render.

The live WS endpoint fans out :data:`eventsub_bus` events for a
broadcaster id — Blue blueprints subscribe here.
"""

from __future__ import annotations

import uuid
from typing import Any

from fastapi import APIRouter, Depends, HTTPException, WebSocket, WebSocketDisconnect, status
from pydantic import BaseModel
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from orion.database import get_session
from orion.models.credential import TwitchCredential
from orion.models.eventsub import EventSubSubscription
from orion.routes._deps import require_authenticated_user
from orion.services import helix_session, twitch_helix
from orion.services.eventsub_bus import eventsub_bus
from orion.services.eventsub_supervisor import supervisor as eventsub_supervisor

router = APIRouter(prefix="/eventsub", tags=["eventsub"])


class EventSubCreate(BaseModel):
    event_type: str
    version: str = "1"
    condition: dict[str, Any]


class EventSubRead(BaseModel):
    id: uuid.UUID
    credential_id: uuid.UUID
    twitch_subscription_id: str
    event_type: str
    version: str
    status: str
    cost: int
    condition: dict[str, Any]

    model_config = {"from_attributes": True}


async def _load_owned_credential(
    db: AsyncSession, credential_id: uuid.UUID, user_id: uuid.UUID
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
    if isinstance(exc, twitch_helix.TwitchAuthError):
        return HTTPException(
            status.HTTP_401_UNAUTHORIZED,
            detail="Twitch rejected the access token. Re-authorize the credential.",
        )
    if isinstance(exc, twitch_helix.TwitchAPIError):
        return HTTPException(status.HTTP_502_BAD_GATEWAY, detail=f"Twitch API error: {exc}")
    if isinstance(exc, TimeoutError):
        return HTTPException(
            status.HTTP_504_GATEWAY_TIMEOUT,
            detail="Twitch EventSub WebSocket did not yield a session id in time.",
        )
    raise exc


# ── Subscription CRUD ─────────────────────────────────────────────────────


@router.post(
    "/credentials/{credential_id}/subscriptions",
    response_model=EventSubRead,
    status_code=status.HTTP_201_CREATED,
)
async def create_subscription(
    credential_id: uuid.UUID,
    payload: EventSubCreate,
    user_id: uuid.UUID = Depends(require_authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> EventSubRead:
    """Create a Twitch EventSub subscription using the credential's OAuth.

    Implementation note : we open (idempotent) the WS supervisor for
    this credential, wait for ``session_welcome``, then submit the
    subscription via Helix referencing ``session.id``.
    """
    cred = await _load_owned_credential(db, credential_id, user_id)
    try:
        await eventsub_supervisor.open(str(cred.id))
        session_id = await eventsub_supervisor.wait_for_session(str(cred.id), timeout=10.0)
        twitch_payload = await helix_session.call_with_credential(
            db,
            cred,
            lambda h: h.create_eventsub_subscription(
                event_type=payload.event_type,
                version=payload.version,
                condition=payload.condition,
                session_id=session_id,
            ),
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc

    row = EventSubSubscription(
        credential_id=cred.id,
        twitch_subscription_id=twitch_payload["id"],
        event_type=twitch_payload.get("type", payload.event_type),
        version=twitch_payload.get("version", payload.version),
        status=twitch_payload.get("status", "enabled"),
        cost=int(twitch_payload.get("cost", 1)),
        condition=twitch_payload.get("condition", payload.condition),
    )
    db.add(row)
    await db.commit()
    return EventSubRead.model_validate(row)


@router.get(
    "/credentials/{credential_id}/subscriptions",
    response_model=list[EventSubRead],
)
async def list_subscriptions(
    credential_id: uuid.UUID,
    user_id: uuid.UUID = Depends(require_authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> list[EventSubRead]:
    cred = await _load_owned_credential(db, credential_id, user_id)
    stmt = (
        select(EventSubSubscription)
        .where(EventSubSubscription.credential_id == cred.id)
        .order_by(EventSubSubscription.created_at.desc())
    )
    result = await db.execute(stmt)
    return [EventSubRead.model_validate(row) for row in result.scalars()]


@router.delete(
    "/subscriptions/{subscription_id}",
    status_code=status.HTTP_204_NO_CONTENT,
)
async def delete_subscription(
    subscription_id: uuid.UUID,
    user_id: uuid.UUID = Depends(require_authenticated_user),
    db: AsyncSession = Depends(get_session),
) -> None:
    row = await db.get(EventSubSubscription, subscription_id)
    if row is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Subscription not found.")
    cred = await db.get(TwitchCredential, row.credential_id)
    if cred is None or cred.owner_id != user_id:
        raise HTTPException(status.HTTP_404_NOT_FOUND, detail="Subscription not found.")

    try:
        await helix_session.call_with_credential(
            db,
            cred,
            lambda h: h.delete_eventsub_subscription(row.twitch_subscription_id),
        )
    except Exception as exc:  # noqa: BLE001 — mapped below
        raise _map_helix_errors(exc) from exc

    await db.delete(row)
    await db.commit()


# ── Live fan-out ──────────────────────────────────────────────────────────


@router.websocket("/live/{broadcaster_id}")
async def live_socket(websocket: WebSocket, broadcaster_id: str) -> None:
    """Stream EventSub notifications for a broadcaster as JSON frames.

    Subscriptions are managed via the CRUD endpoints above ; this
    socket is a passive consumer of :data:`eventsub_bus`. If no
    subscription is active for the broadcaster, the subscriber sees
    an empty stream — never an error.
    """
    await websocket.accept()
    queue = eventsub_bus.subscribe(broadcaster_id)
    try:
        while True:
            event = await queue.get()
            await websocket.send_json(event)
    except WebSocketDisconnect:
        return
    finally:
        eventsub_bus.unsubscribe(broadcaster_id, queue)
