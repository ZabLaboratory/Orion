"""Internal endpoints — _schema and _query (read-only).

Orion holds AES-GCM ciphertext for Twitch stream keys and OAuth tokens
on the ``twitch_credentials`` table — that table is intentionally
omitted from the catalog. Only ``chat_messages`` is exposed at this
stage ; expanding the catalog is gated by an explicit security review
each time a new column reaches blueprint readers.

Tests prove the conservatism is enforced, not just declared.
"""

from __future__ import annotations

from httpx import AsyncClient

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
    must not appear in the catalog at all."""
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
    # Channel-driven columns still present.
    assert {"channel", "author_login", "content", "sent_at"} <= column_names


# ── Validation rejection ──────────────────────────────────────────────────


async def test_query_against_twitch_credentials_returns_400(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/_query",
        json={"table": "twitch_credentials", "select": ["id"]},
    )
    assert resp.status_code == 400
    issues = resp.json()["detail"]["issues"]
    assert issues[0]["code"] == "unknown_table"


async def test_query_against_dropped_streams_table_returns_400(
    client: AsyncClient,
) -> None:
    """``streams`` was dropped at the pivot ; lingering catalog references
    or accidental client retries surface as a clean 400."""
    resp = await client.post(
        "/api/v1/_query",
        json={"table": "streams", "select": ["id"]},
    )
    assert resp.status_code == 400
    issues = resp.json()["detail"]["issues"]
    assert issues[0]["code"] == "unknown_table"


# ── Read-only contract ────────────────────────────────────────────────────


async def test_no_mutate_endpoint_exists(client: AsyncClient) -> None:
    resp = await client.post("/api/v1/_mutate", json={})
    assert resp.status_code == 404
