"""Application configuration."""

from pathlib import Path

from pydantic_settings import BaseSettings

_ENV_FILE = Path(__file__).resolve().parents[3] / ".env.orion"


class Settings(BaseSettings):
    """Orion runtime settings. Loaded from ../.env.orion (etage 1)."""

    debug: bool = False

    # Database
    database_url: str = "postgresql+asyncpg://orion:orion_dev@localhost:5447/orion"

    # Cryptography — 32-byte key, urlsafe-b64 encoded. Used to encrypt Twitch stream keys.
    # Generate: python -c "import secrets,base64; print(base64.urlsafe_b64encode(secrets.token_bytes(32)).decode())"
    encryption_key: str = ""

    # MediaMTX — internal media engine
    mediamtx_api_url: str = "http://orion-mediamtx:9997"
    mediamtx_whip_base: str = "http://orion-mediamtx:8889"
    mediamtx_public_whip_base: str = "http://localhost:8889"  # what the browser hits

    # Twitch RTMP ingest (no trailing slash)
    twitch_rtmp_base: str = "rtmp://live.twitch.tv/app"

    # Twitch Helix / OAuth (optional — needed for channel info + chat)
    twitch_client_id: str = ""
    twitch_client_secret: str = ""
    twitch_oauth_redirect_uri: str = "http://localhost:3000/settings/twitch/callback"

    # Public base URL — used to build WHIP URLs returned to browsers
    public_base_url: str = "http://localhost:4007"

    # Ingress token TTL (seconds) — short-lived single-use token embedded in WHIP URL
    ingress_token_ttl_seconds: int = 300

    model_config = {
        "env_file": str(_ENV_FILE) if _ENV_FILE.exists() else None,
        "extra": "ignore",
    }


settings = Settings()
