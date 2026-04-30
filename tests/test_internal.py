"""Internal endpoints — _schema and _query (read-only).

Orion holds AES-GCM ciphertext for Twitch stream keys and OAuth tokens
on the ``twitch_credentials`` table — that table is intentionally
omitted from the catalogue. Only ``chat_messages`` is exposed at this
stage ; expanding the catalogue is gated by an explicit security review
each time a new column reaches blueprint readers.

``_query`` is hardened : auth required (``X-Authenticated-User``), rate
limited per user, and every accepted query emits a JSON audit line.
"""

from __future__ import annotations

import json

import pytest
from httpx import AsyncClient

from orion.routes import internal as internal_routes

from .conftest import TEST_USER_ID

# ── Reset state between tests ──────────────────────────────────────────────


@pytest.fixture(autouse=True)
def _reset_rate_limiter() -> None:
    internal_routes._rate_buckets.clear()


# ── Schema shape ──────────────────────────────────────────────────────────


async def test_schema_returns_orion_catalog(client: AsyncClient) -> None:
    resp = await client.get("/api/v1/_schema")
    assert resp.status_code == 200
    body = resp.json()
    assert body["service"] == "orion"
    table_names = {t["name"] for t in body["tables"]}
    assert table_names == {"chat_messages"}


async def test_schema_omits_twitch_credentials_entirely(client: AsyncClient) -> None:
    """The whole table is sensitive (AES-GCM ciphertext + OAuth) — it
    must not appear in the catalogue at all."""
    resp = await client.get("/api/v1/_schema")
    table_names = {t["name"] for t in resp.json()["tables"]}
    assert "twitch_credentials" not in table_names


async def test_schema_marks_all_tables_read_only(client: AsyncClient) -> None:
    resp = await client.get("/api/v1/_schema")
    for table in resp.json()["tables"]:
        assert table["writable"] is False, f"{table['name']} should be read-only"


async def test_schema_chat_messages_carries_no_streaming_residue(
    client: AsyncClient,
) -> None:
    """Post-pivot, chat_messages is channel-keyed only. ``stream_id``
    and any other streaming-era column must be gone."""
    resp = await client.get("/api/v1/_schema")
    chat_table = next(t for t in resp.json()["tables"] if t["name"] == "chat_messages")
    column_names = {c["name"] for c in chat_table["columns"]}
    assert "stream_id" not in column_names
    assert {"channel", "author_login", "content", "sent_at"} <= column_names


async def test_schema_does_not_require_auth(client: AsyncClient) -> None:
    """Schema is the catalogue itself — no rows leak. Stays public."""
    resp = await client.get("/api/v1/_schema")
    assert resp.status_code == 200


# ── Query — happy paths (require auth) ─────────────────────────────────────


async def test_query_chat_messages_basic_select(auth_client: AsyncClient) -> None:
    resp = await auth_client.post(
        "/api/v1/_query",
        json={"table": "chat_messages", "select": ["channel", "author_login"]},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["count"] == 0
    assert body["rows"] == []


# ── Validation rejection (still requires auth) ─────────────────────────────


async def test_query_against_twitch_credentials_returns_400(
    auth_client: AsyncClient,
) -> None:
    resp = await auth_client.post(
        "/api/v1/_query",
        json={"table": "twitch_credentials", "select": ["id"]},
    )
    assert resp.status_code == 400
    issues = resp.json()["detail"]["issues"]
    assert issues[0]["code"] == "unknown_table"


async def test_query_against_dropped_streams_table_returns_400(
    auth_client: AsyncClient,
) -> None:
    """``streams`` was dropped at the pivot ; lingering catalogue references
    or accidental client retries surface as a clean 400."""
    resp = await auth_client.post(
        "/api/v1/_query",
        json={"table": "streams", "select": ["id"]},
    )
    assert resp.status_code == 400
    issues = resp.json()["detail"]["issues"]
    assert issues[0]["code"] == "unknown_table"


# ── Hardening : auth ───────────────────────────────────────────────────────


async def test_query_rejects_anonymous(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/_query",
        json={"table": "chat_messages", "select": ["channel"]},
    )
    assert resp.status_code == 401


async def test_query_rejects_malformed_user_header(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/_query",
        json={"table": "chat_messages", "select": ["channel"]},
        headers={"X-Authenticated-User": "not-a-uuid"},
    )
    # `_deps.authenticated_user` swallows malformed UUIDs and returns
    # None ; require_authenticated_user then raises 401.
    assert resp.status_code == 401


# ── Hardening : rate limit ─────────────────────────────────────────────────


async def test_query_rate_limit_returns_429(
    auth_client: AsyncClient, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(internal_routes, "RATE_LIMIT_MAX_REQUESTS", 3)
    monkeypatch.setattr(internal_routes, "RATE_LIMIT_WINDOW_SECONDS", 60.0)
    internal_routes._rate_buckets.clear()

    payload = {"table": "chat_messages", "select": ["channel"]}
    for _ in range(3):
        resp = await auth_client.post("/api/v1/_query", json=payload)
        assert resp.status_code == 200, resp.text
    blocked = await auth_client.post("/api/v1/_query", json=payload)
    assert blocked.status_code == 429
    assert "Retry-After" in blocked.headers
    assert blocked.json()["detail"]["retry_after_seconds"] >= 1


async def test_query_rate_limit_is_per_user(
    auth_client: AsyncClient, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Saturating one user's bucket must not affect another user."""
    monkeypatch.setattr(internal_routes, "RATE_LIMIT_MAX_REQUESTS", 2)
    internal_routes._rate_buckets.clear()

    payload = {"table": "chat_messages", "select": ["channel"]}
    for _ in range(2):
        resp = await auth_client.post("/api/v1/_query", json=payload)
        assert resp.status_code == 200
    blocked = await auth_client.post("/api/v1/_query", json=payload)
    assert blocked.status_code == 429

    other_user = "22222222-2222-2222-2222-222222222222"
    other = await auth_client.post(
        "/api/v1/_query",
        json=payload,
        headers={"X-Authenticated-User": other_user},
    )
    assert other.status_code == 200, other.text


# ── Hardening : audit log ──────────────────────────────────────────────────


async def test_query_emits_audit_log(
    auth_client: AsyncClient, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level("INFO", logger="orion.queryme.audit")
    resp = await auth_client.post(
        "/api/v1/_query",
        json={"table": "chat_messages", "select": ["channel"]},
    )
    assert resp.status_code == 200

    audit_records = [r for r in caplog.records if r.name == "orion.queryme.audit"]
    assert audit_records, "expected at least one audit log line"
    payload = json.loads(audit_records[-1].getMessage())
    assert payload["event"] == "queryme.query"
    assert payload["user_id"] == TEST_USER_ID
    assert payload["table"] == "chat_messages"
    assert payload["joins"] == []
    assert isinstance(payload["elapsed_ms"], int)


# ── Read-only contract ────────────────────────────────────────────────────


async def test_no_mutate_endpoint_exists(client: AsyncClient) -> None:
    resp = await client.post("/api/v1/_mutate", json={})
    assert resp.status_code == 404
