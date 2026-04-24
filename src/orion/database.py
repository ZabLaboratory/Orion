"""Database engine and async session factory."""

from collections.abc import AsyncIterator

from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker, create_async_engine

from orion.config import settings

engine = create_async_engine(settings.database_url, echo=settings.debug)
async_session = async_sessionmaker(engine, class_=AsyncSession, expire_on_commit=False)


async def get_session() -> AsyncIterator[AsyncSession]:
    """FastAPI dependency: yield an async session scoped to the request."""
    async with async_session() as session:
        yield session
