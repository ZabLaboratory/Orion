"""Static schema declaration for Orion's blueprint-facing surface.

Read-only per ADR 001 §1. Stream / credential / destination authoring
goes through the existing CRUD routes — keeping all crypto behind
``services/encryption.py`` and the single-writer-on-state rule on
``stream_manager`` intact.

Security model — the same column-level filtering by absence used in
ZabAuth and ZabCam, applied aggressively here because Orion holds
both AES-GCM ciphertext (Twitch stream keys, OAuth tokens, RTMP
credentials) and runtime-sensitive tokens (ingress tokens) :

- ``twitch_credentials`` is **omitted entirely** — every interesting
  column on it is sensitive (ciphertext / nonce / scopes) and the
  blueprint surface has no legitimate read use case yet. When one
  arises we'll expose a derived "summary" view (label, channel_login,
  has_oauth) ; until then default-deny.
- ``stream_destinations`` is exposed but **only its safe metadata
  columns** are listed (kind, display_name, enabled, ordering). The
  ciphertext / nonce columns for rtmp_url and stream_key are not in
  the catalog.
- ``streams.ingress_token`` is **omitted** — it is a runtime
  credential the broadcaster posts back ; exposure would let any
  blueprint read enrol-tokens out of the DB.

Other tables (``chat_messages``, ``stream_metrics``) carry no
secrets and are exposed in full.
"""

from __future__ import annotations

from queryme import (
    ColumnDef,
    RelationDef,
    SchemaDescriptor,
    TableDef,
)

CATALOG: SchemaDescriptor = SchemaDescriptor(
    service="orion",
    tables=[
        TableDef(
            name="streams",
            writable=False,
            description=(
                "Broadcast sessions — lifecycle pending / preparing / live "
                "/ stopping / ended / error. ingress_token is intentionally "
                "absent from the catalog (runtime credential, must not "
                "leak through generic queries)."
            ),
            columns=[
                ColumnDef(name="id", type="uuid", primary=True),
                ColumnDef(name="owner_id", type="uuid", nullable=True),
                ColumnDef(name="overlay_id", type="uuid", nullable=True),
                ColumnDef(name="overlay_playlist", type="json"),
                ColumnDef(name="credential_id", type="uuid"),
                # StreamState is a PG enum on the wire ; QueryMe maps it
                # to string.
                ColumnDef(name="state", type="string"),
                ColumnDef(name="mediamtx_path", type="string"),
                ColumnDef(name="whip_endpoint", type="string"),
                # ingress_token / ingress_token_expires_at omitted on
                # purpose (runtime credentials).
                ColumnDef(name="twitch_stream_id", type="string", nullable=True),
                ColumnDef(name="target_width", type="integer"),
                ColumnDef(name="target_height", type="integer"),
                ColumnDef(name="target_fps", type="integer"),
                ColumnDef(name="video_bitrate_kbps", type="integer"),
                ColumnDef(name="audio_bitrate_kbps", type="integer"),
                ColumnDef(name="keyframe_interval_s", type="integer"),
                ColumnDef(name="encoder_preset", type="string"),
                ColumnDef(name="record", type="boolean"),
                ColumnDef(name="started_at", type="datetime", nullable=True),
                ColumnDef(name="ended_at", type="datetime", nullable=True),
                ColumnDef(name="error_message", type="string", nullable=True),
                ColumnDef(name="metadata", type="json"),
                ColumnDef(name="created_at", type="datetime"),
                ColumnDef(name="updated_at", type="datetime"),
            ],
        ),
        TableDef(
            name="stream_destinations",
            writable=False,
            description=(
                "Per-stream output targets. RTMP URL and stream key live "
                "in the row as AES-GCM ciphertext but are absent from "
                "this catalog — only safe metadata is queryable."
            ),
            columns=[
                ColumnDef(name="id", type="uuid", primary=True),
                ColumnDef(name="stream_id", type="uuid"),
                ColumnDef(name="kind", type="string"),
                ColumnDef(name="display_name", type="string"),
                ColumnDef(name="enabled", type="boolean"),
                ColumnDef(name="ordering", type="integer"),
                ColumnDef(name="created_at", type="datetime"),
                ColumnDef(name="updated_at", type="datetime"),
                # rtmp_url_ciphertext / rtmp_url_nonce / stream_key_ciphertext
                # / stream_key_nonce are omitted on purpose.
            ],
            relations=[
                RelationDef(to="streams", via="stream_id", kind="belongs_to"),
            ],
        ),
        TableDef(
            name="chat_messages",
            writable=False,
            description="Captured Twitch IRC messages for replay / audit.",
            columns=[
                ColumnDef(name="id", type="integer", primary=True),
                ColumnDef(name="stream_id", type="uuid", nullable=True),
                ColumnDef(name="channel", type="string"),
                ColumnDef(name="author_login", type="string"),
                ColumnDef(name="author_id", type="string", nullable=True),
                ColumnDef(name="author_display", type="string", nullable=True),
                ColumnDef(name="content", type="text"),
                ColumnDef(name="badges", type="json"),
                ColumnDef(name="emotes", type="json"),
                ColumnDef(name="sent_at", type="datetime"),
                ColumnDef(name="created_at", type="datetime"),
            ],
            relations=[
                RelationDef(to="streams", via="stream_id", kind="belongs_to"),
            ],
        ),
        TableDef(
            name="stream_metrics",
            writable=False,
            description="Time-series : bitrate, fps, dropped frames, rtt, viewers.",
            columns=[
                ColumnDef(name="id", type="integer", primary=True),
                ColumnDef(name="stream_id", type="uuid"),
                ColumnDef(name="ts", type="datetime"),
                ColumnDef(name="bitrate_kbps", type="integer", nullable=True),
                ColumnDef(name="fps", type="integer", nullable=True),
                ColumnDef(name="dropped_frames", type="integer", nullable=True),
                ColumnDef(name="rtt_ms", type="integer", nullable=True),
                ColumnDef(name="viewers", type="integer", nullable=True),
                ColumnDef(name="raw", type="json"),
            ],
            relations=[
                RelationDef(to="streams", via="stream_id", kind="belongs_to"),
            ],
        ),
        # twitch_credentials is intentionally not in this catalog — see
        # module docstring. All its columns are sensitive ciphertext or
        # secret metadata that has no legitimate generic-read use case.
    ],
)
