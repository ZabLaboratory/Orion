"""MediaMTX HTTP API client.

Orion talks to MediaMTX to:
- Create dynamic paths when a stream is started
- Inject the Twitch RTMP push as ``runOnReady``
- Poll path state to detect when the stream goes live / idle
- Delete paths when the stream ends

API reference: https://bluenviron.github.io/mediamtx/ — v3 control API.
"""

from __future__ import annotations

import logging
from typing import Any

import httpx

from orion.config import settings

logger = logging.getLogger(__name__)


class MediaMTXError(RuntimeError):
    """Non-2xx response from MediaMTX."""


class MediaMTXClient:
    """Thin async client around the MediaMTX v3 control API.

    Reused across requests — caller is responsible for ``aclose()``.
    """

    def __init__(self, base_url: str | None = None, timeout: float = 5.0) -> None:
        self._base = (base_url or settings.mediamtx_api_url).rstrip("/")
        self._client = httpx.AsyncClient(base_url=self._base, timeout=timeout)

    async def aclose(self) -> None:
        await self._client.aclose()

    # ------------------------------------------------------------------ paths
    async def add_path(self, name: str, config: dict[str, Any]) -> None:
        """Create a MediaMTX path. Fails if the path already exists."""
        r = await self._client.post(f"/v3/config/paths/add/{name}", json=config)
        if r.status_code >= 300:
            logger.error("MediaMTX add_path failed: %s %s", r.status_code, r.text)
            raise MediaMTXError(f"add_path({name}) -> {r.status_code}: {r.text}")

    async def replace_path(self, name: str, config: dict[str, Any]) -> None:
        """Create-or-update a MediaMTX path."""
        r = await self._client.post(f"/v3/config/paths/replace/{name}", json=config)
        if r.status_code >= 300:
            raise MediaMTXError(f"replace_path({name}) -> {r.status_code}: {r.text}")

    async def delete_path(self, name: str) -> None:
        """Remove a MediaMTX path. No-op if it does not exist."""
        r = await self._client.delete(f"/v3/config/paths/delete/{name}")
        if r.status_code == 404:
            return
        if r.status_code >= 300:
            logger.warning("MediaMTX delete_path non-2xx: %s %s", r.status_code, r.text)

    async def list_paths(self) -> list[dict[str, Any]]:
        r = await self._client.get("/v3/paths/list")
        r.raise_for_status()
        payload = r.json()
        items = payload.get("items", []) if isinstance(payload, dict) else []
        if isinstance(items, list):
            return items
        return []

    async def get_path(self, name: str) -> dict[str, Any] | None:
        r = await self._client.get(f"/v3/paths/get/{name}")
        if r.status_code == 404:
            return None
        r.raise_for_status()
        data = r.json()
        return data if isinstance(data, dict) else None


def build_twitch_relay_config(stream_key: str, path_name: str) -> dict[str, Any]:
    """Build a MediaMTX path config that accepts WHIP and pushes RTMP to Twitch.

    The browser publisher sends WebRTC's default codecs (VP8 + Opus). Twitch's
    RTMP ingest only accepts H264 + AAC, so we can't ``-c copy`` — ffmpeg has
    to transcode on the VPS:

    - ``libx264 -preset veryfast`` : CPU-only (VPS has no GPU); veryfast gives
      a reasonable quality/CPU trade-off for 1080p30 at 6 Mbps.
    - ``-g 60 -keyint_min 60 -sc_threshold 0`` : 2-second keyframe interval at
      30 fps (Twitch's requirement; they drop the stream otherwise).
    - ``-b:v 6000k -maxrate 6000k -bufsize 12000k`` : Twitch Partner tier bitrate
      cap; drop to 4500/9000 if we ever add a sub-Partner profile.
    - Audio transcoded to AAC 160 kbps stereo 48 kHz — universal Twitch baseline.

    ``runOnReadyRestart`` keeps the child alive across a brief publisher drop
    (ICE restart, WiFi blip) — MediaMTX respawns ffmpeg as soon as the path is
    ready again.
    """
    twitch_url = f"{settings.twitch_rtmp_base}/{stream_key}"
    ffmpeg_cmd = (
        "ffmpeg -hide_banner -loglevel warning "
        "-fflags nobuffer -rtsp_transport tcp "
        f"-i rtsp://localhost:8554/{path_name} "
        "-c:v libx264 -preset veryfast -pix_fmt yuv420p "
        "-b:v 6000k -maxrate 6000k -bufsize 12000k "
        "-g 60 -keyint_min 60 -sc_threshold 0 "
        "-c:a aac -b:a 160k -ar 48000 -ac 2 "
        f"-f flv {twitch_url}"
    )
    return {
        "source": "publisher",
        "sourceOnDemand": False,
        "runOnReady": ffmpeg_cmd,
        "runOnReadyRestart": True,
    }
