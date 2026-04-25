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
from dataclasses import dataclass
from typing import Any

import httpx

from orion.config import settings

logger = logging.getLogger(__name__)


class MediaMTXError(RuntimeError):
    """Non-2xx response from MediaMTX."""


@dataclass(frozen=True)
class StreamingParams:
    """Knobs that drive ffmpeg's transcode of the WebRTC ingest into Twitch RTMP."""

    width: int = 1920
    height: int = 1080
    fps: int = 30
    video_bitrate_kbps: int = 6000
    audio_bitrate_kbps: int = 160
    keyframe_interval_s: int = 2
    encoder_preset: str = "veryfast"


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


def build_twitch_relay_config(
    stream_key: str,
    path_name: str,
    params: StreamingParams | None = None,
) -> dict[str, Any]:
    """Build a MediaMTX path config that accepts WHIP and pushes RTMP to Twitch.

    The browser publisher sends WebRTC's default codecs (VP8 + Opus). Twitch's
    RTMP ingest only accepts H264 + AAC, so we can't ``-c copy`` — ffmpeg has
    to transcode on the VPS. ``StreamingParams`` (set on the Stream row by
    Orion's API) drives every encoder knob:

    - ``-c:v libx264 -preset <encoder_preset>`` : CPU-only (VPS has no GPU);
      ``veryfast`` is a reasonable default for 1080p30 at 6 Mbps.
    - ``-s WIDTHxHEIGHT -r FPS`` : ffmpeg downscales / drops frames if the
      ingest sends something larger; matches Twitch's recommended ladder.
    - ``-g <keyframe_interval_s * fps>`` : Twitch enforces a hard 2 s
      keyframe interval; default is 60 (= 2 s @ 30 fps).
    - ``-b:v / -maxrate / -bufsize`` : capped at the configured bitrate;
      Twitch Partner tier caps at 6000 kbps, Affiliate at 8000 kbps.
    - ``-c:a aac -b:a <audio_bitrate_kbps> -ar 48000 -ac 2`` : universal
      Twitch baseline (160 kbps stereo 48 kHz default).

    ``runOnReadyRestart`` keeps the child alive across a brief publisher drop
    (ICE restart, WiFi blip) — MediaMTX respawns ffmpeg as soon as the path is
    ready again.
    """
    p = params or StreamingParams()
    twitch_url = f"{settings.twitch_rtmp_base}/{stream_key}"
    keyint = max(1, p.keyframe_interval_s * p.fps)
    bufsize_kbps = p.video_bitrate_kbps * 2
    ffmpeg_cmd = (
        "ffmpeg -hide_banner -loglevel warning "
        "-fflags nobuffer -rtsp_transport tcp "
        f"-i rtsp://localhost:8554/{path_name} "
        f"-c:v libx264 -preset {p.encoder_preset} -pix_fmt yuv420p "
        f"-s {p.width}x{p.height} -r {p.fps} "
        f"-b:v {p.video_bitrate_kbps}k -maxrate {p.video_bitrate_kbps}k -bufsize {bufsize_kbps}k "
        f"-g {keyint} -keyint_min {keyint} -sc_threshold 0 "
        f"-c:a aac -b:a {p.audio_bitrate_kbps}k -ar 48000 -ac 2 "
        f"-f flv {twitch_url}"
    )
    return {
        "source": "publisher",
        "sourceOnDemand": False,
        "runOnReady": ffmpeg_cmd,
        "runOnReadyRestart": True,
    }
