"""Application configuration."""

from pathlib import Path

from pydantic_settings import BaseSettings

_ENV_FILE = Path(__file__).resolve().parents[3] / ".env.orion"


class Settings(BaseSettings):
    """Orion runtime settings. Loaded from ../.env.orion (etage 1)."""

    debug: bool = False

    # Database
    database_url: str = "postgresql+asyncpg://orion:orion_dev@localhost:5447/orion"

    # Cryptography — 32-byte key, urlsafe-b64 encoded. Used to encrypt Twitch
    # stream keys and OAuth tokens at rest.
    # Generate: python -c "import secrets,base64; print(base64.urlsafe_b64encode(secrets.token_bytes(32)).decode())"
    encryption_key: str = ""

    # Twitch Helix / OAuth
    twitch_client_id: str = ""
    twitch_client_secret: str = ""
    twitch_oauth_redirect_uri: str = "http://localhost:3000/settings/twitch/callback"

    model_config = {
        "env_file": str(_ENV_FILE) if _ENV_FILE.exists() else None,
        "extra": "ignore",
    }


settings = Settings()
