"""Shared pytest fixtures."""

import os
from collections.abc import AsyncIterator

import pytest
import pytest_asyncio
from httpx import ASGITransport, AsyncClient
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker, create_async_engine

os.environ.setdefault("DATABASE_URL", "sqlite+aiosqlite:///:memory:")
os.environ.setdefault(
    # Deterministic 32-byte key used only in tests. NEVER reuse in prod.
    "ENCRYPTION_KEY",
    "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
)

from orion.main import app  # noqa: E402
from orion.models import Base  # noqa: E402


@pytest_asyncio.fixture
async def db_session() -> AsyncIterator[AsyncSession]:
    engine = create_async_engine("sqlite+aiosqlite:///:memory:", echo=False)
    async with engine.begin() as conn:
        await conn.run_sync(Base.metadata.create_all)
    session_factory = async_sessionmaker(engine, class_=AsyncSession, expire_on_commit=False)
    async with session_factory() as session:
        yield session
    await engine.dispose()


@pytest_asyncio.fixture
async def client() -> AsyncIterator[AsyncClient]:
    transport = ASGITransport(app=app)
    async with AsyncClient(transport=transport, base_url="http://testserver") as c:
        yield c


@pytest.fixture
def authenticated_headers() -> dict[str, str]:
    return {"x-authenticated-user": "11111111-1111-1111-1111-111111111111"}
