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

from orion.database import engine as _app_engine  # noqa: E402
from orion.main import app  # noqa: E402
from orion.models import Base  # noqa: E402


@pytest_asyncio.fixture(autouse=True)
async def _setup_app_db() -> AsyncIterator[None]:
    """Ensure the FastAPI app's global engine has tables.

    The global engine points at the in-memory sqlite database (set via
    ``DATABASE_URL`` above) ; tests that touch ``get_session`` need
    those tables to exist. Reset between tests so state never leaks.
    """
    async with _app_engine.begin() as conn:
        await conn.run_sync(Base.metadata.create_all)
    yield
    async with _app_engine.begin() as conn:
        await conn.run_sync(Base.metadata.drop_all)


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


TEST_USER_ID = "11111111-1111-1111-1111-111111111111"


@pytest_asyncio.fixture
async def auth_client() -> AsyncIterator[AsyncClient]:
    """Client with the gateway-injected identity header pre-set.

    Use for endpoints that require ``X-Authenticated-User`` (currently
    only the QueryMe ``_query`` surface).
    """
    transport = ASGITransport(app=app)
    headers = {"X-Authenticated-User": TEST_USER_ID}
    async with AsyncClient(transport=transport, base_url="http://testserver", headers=headers) as c:
        yield c


@pytest.fixture
def authenticated_headers() -> dict[str, str]:
    return {"x-authenticated-user": TEST_USER_ID}
