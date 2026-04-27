"""Internal endpoints — _schema and _query (read-only).

Orion is the most security-sensitive service in the read-only fan-out :
it holds AES-GCM ciphertext for Twitch stream keys, OAuth tokens, and
RTMP destinations. The catalog is conservative on purpose ; tests
prove the conservatism is enforced, not just declared.

DB-touching tests are skipped here because Orion's models lean on
PostgreSQL-specific types (ARRAY, JSONB) ; e2e validation lives in
the production smoke step. What matters at this layer :

- Catalog shape : which tables / columns are exposed.
- Validation rejection : sensitive columns return 400, not silent
  empty rows.
- ``_mutate`` is absent : read-only contract holds.
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
    assert table_names == {"streams", "stream_destinations", "chat_messages", "stream_metrics"}


async def test_schema_omits_twitch_credentials_entirely(client: AsyncClient) -> None:
    """The whole table is sensitive (AES-GCM ciphertext + OAuth) — it
    must not appear in the catalog at all."""
    resp = await client.get("/api/v1/_schema")
    table_names = {t["name"] for t in resp.json()["tables"]}
    assert "twitch_credentials" not in table_names


async def test_schema_streams_omits_ingress_token(client: AsyncClient) -> None:
    """ingress_token is a runtime-sensitive credential — broadcasters
    re-post it, so leaking it via a generic SELECT would let any
    blueprint impersonate a streamer at startup."""
    resp = await client.get("/api/v1/_schema")
    streams = next(t for t in resp.json()["tables"] if t["name"] == "streams")
    column_names = {c["name"] for c in streams["columns"]}
    assert "ingress_token" not in column_names
    assert "ingress_token_expires_at" not in column_names


async def test_schema_destinations_omits_ciphertext_columns(client: AsyncClient) -> None:
    """RTMP URLs and destination stream keys are AES-GCM ciphertext.
    Even ciphertext shouldn't be available for offline crypto attacks
    or chain-of-custody compromise."""
    resp = await client.get("/api/v1/_schema")
    destinations = next(
        t for t in resp.json()["tables"] if t["name"] == "stream_destinations"
    )
    column_names = {c["name"] for c in destinations["columns"]}
    assert "rtmp_url_ciphertext" not in column_names
    assert "rtmp_url_nonce" not in column_names
    assert "stream_key_ciphertext" not in column_names
    assert "stream_key_nonce" not in column_names
    # Safe metadata is still queryable :
    assert {"kind", "display_name", "enabled", "ordering"} <= column_names


async def test_schema_marks_all_tables_read_only(client: AsyncClient) -> None:
    resp = await client.get("/api/v1/_schema")
    for table in resp.json()["tables"]:
        assert table["writable"] is False, f"{table['name']} should be read-only"


# ── Validation rejection (DB-less — just exercises the validator) ─────────


async def test_query_against_twitch_credentials_returns_400(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/_query",
        json={"table": "twitch_credentials", "select": ["id"]},
    )
    assert resp.status_code == 400
    issues = resp.json()["detail"]["issues"]
    assert issues[0]["code"] == "unknown_table"


async def test_query_cannot_select_ingress_token(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/_query",
        json={"table": "streams", "select": ["id", "ingress_token"]},
    )
    assert resp.status_code == 400
    issues = resp.json()["detail"]["issues"]
    assert any(
        i["code"] == "unknown_select_column" and "ingress_token" in i["message"]
        for i in issues
    )


async def test_query_cannot_select_destination_ciphertext(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/_query",
        json={
            "table": "stream_destinations",
            "select": ["id", "stream_key_ciphertext"],
        },
    )
    assert resp.status_code == 400
    issues = resp.json()["detail"]["issues"]
    assert any(i["code"] == "unknown_select_column" for i in issues)


async def test_query_cannot_filter_on_ingress_token(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/_query",
        json={
            "table": "streams",
            "select": ["id"],
            "where": [{"column": "ingress_token", "op": "=", "value": "tok"}],
        },
    )
    assert resp.status_code == 400
    issues = resp.json()["detail"]["issues"]
    assert any(i["code"] == "unknown_column" for i in issues)


# ── Read-only contract ────────────────────────────────────────────────────


async def test_no_mutate_endpoint_exists(client: AsyncClient) -> None:
    resp = await client.post("/api/v1/_mutate", json={})
    assert resp.status_code == 404
