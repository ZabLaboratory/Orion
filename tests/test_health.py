"""Health endpoint smoke test."""

from httpx import AsyncClient


async def test_health_returns_service_name(client: AsyncClient) -> None:
    res = await client.get("/health")
    # SQLite may or may not succeed against the default engine; what matters is the
    # service-identifying payload is there.
    assert res.status_code in (200, 503)
    body = res.json()
    assert body["service"] == "orion"
    assert "database" in body
