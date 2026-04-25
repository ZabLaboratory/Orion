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
    *,
    record: bool = False,
    stream_id: str | None = None,
    extra_destinations: list[str] | None = None,
) -> dict[str, Any]:
    """Build a MediaMTX path config that accepts WHIP and fans out via ffmpeg.

    The browser publisher sends WebRTC's default codecs (VP8 + Opus). Twitch's
    RTMP ingest only accepts H264 + AAC, so we can't ``-c copy`` — ffmpeg has
    to transcode on the VPS. ``StreamingParams`` (set on the Stream row by
    Orion's API) drives every encoder knob.

    The legacy single-Twitch path is preserved: when ``record=False`` and
    ``extra_destinations`` is empty, ffmpeg uses a plain ``-f flv <twitch_url>``
    output. Otherwise we switch to the ``tee`` pseudo-muxer with one branch
    per output (Twitch + extras + optional MP4 recording). Each tee branch
    carries ``onfail=ignore`` so a single output's failure (Twitch flap,
    YouTube reject, disk full) doesn't kill the others.

    ``runOnReadyRestart`` keeps the child alive across a brief publisher drop
    (ICE restart, WiFi blip) — MediaMTX respawns ffmpeg as soon as the path is
    ready again. Each respawn opens a fresh recording file (timestamped at
    spawn time) so the previous segment is preserved intact.

    ``extra_destinations`` is a list of fully-qualified RTMP URLs *with the
    stream key already appended* (e.g. ``rtmp://a.rtmp.youtube.com/live2/<key>``).
    The caller is responsible for joining url + key — Orion's
    ``stream_manager`` does this from decrypted ``StreamDestination`` rows.
    """
    p = params or StreamingParams()
    twitch_url = f"{settings.twitch_rtmp_base}/{stream_key}"
    keyint = max(1, p.keyframe_interval_s * p.fps)
    bufsize_kbps = p.video_bitrate_kbps * 2

    # Common encoder block — emitted once regardless of output count.
    encoder_args = (
        f"-c:v libx264 -preset {p.encoder_preset} -pix_fmt yuv420p "
        f"-s {p.width}x{p.height} -r {p.fps} "
        f"-b:v {p.video_bitrate_kbps}k -maxrate {p.video_bitrate_kbps}k -bufsize {bufsize_kbps}k "
        f"-g {keyint} -keyint_min {keyint} -sc_threshold 0 "
        f"-c:a aac -b:a {p.audio_bitrate_kbps}k -ar 48000 -ac 2 "
    )

    extras = list(extra_destinations or [])
    needs_tee = record or bool(extras)

    if not needs_tee:
        # Single output — keep the legacy code path. Smaller blast radius.
        ffmpeg_cmd = (
            "ffmpeg -hide_banner -loglevel warning "
            "-fflags nobuffer -rtsp_transport tcp "
            f"-i rtsp://localhost:8554/{path_name} "
            f"{encoder_args}-f flv {twitch_url}"
        )
    else:
        # Tee pseudo-muxer: single encode, N outputs. ``onfail=ignore`` so any
        # one branch dying doesn't take the others with it.
        branches: list[str] = [f"[f=flv:onfail=ignore]{twitch_url}"]
        for url in extras:
            branches.append(f"[f=flv:onfail=ignore]{url}")

        record_path: str | None = None
        if record and stream_id:
            from datetime import UTC, datetime
            ts = datetime.now(UTC).strftime("%Y-%m-%d_%H-%M-%S")
            record_path = f"/recordings/{stream_id}/{ts}.mp4"
            branches.append(
                f"[f=mp4:onfail=ignore:movflags=+faststart]{record_path}"
            )

        tee_arg = "|".join(branches)
        # ``-flags +global_header`` is required by tee'd MP4 + flv branches —
        # the MP4 muxer needs SPS/PPS in extradata, not inline.
        output_args = f'-flags +global_header -f tee "{tee_arg}"'

        if record_path:
            ffmpeg_cmd = (
                f"sh -c 'mkdir -p /recordings/{stream_id} && "
                f"exec ffmpeg -hide_banner -loglevel warning "
                f"-fflags nobuffer -rtsp_transport tcp "
                f"-i rtsp://localhost:8554/{path_name} "
                f"{encoder_args}{output_args}'"
            )
        else:
            # No recording → skip the mkdir, run ffmpeg directly. Same
            # tee command, just no MP4 branch.
            ffmpeg_cmd = (
                f"ffmpeg -hide_banner -loglevel warning "
                f"-fflags nobuffer -rtsp_transport tcp "
                f"-i rtsp://localhost:8554/{path_name} "
                f"{encoder_args}{output_args}"
            )

    return {
        "source": "publisher",
        "sourceOnDemand": False,
        "runOnReady": ffmpeg_cmd,
        "runOnReadyRestart": True,
    }
