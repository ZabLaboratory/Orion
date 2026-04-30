"""drop streaming surface — pivot to Twitch orchestrator

Revision ID: 0006_drop_streaming
Revises: 0005_destinations
Create Date: 2026-04-30 00:00:00.000000

Big bang teardown : Orion stops being a streaming control plane and
becomes a pure Twitch orchestrator (credentials, OAuth, chat). The
broadcast media path now runs through Pulsar (bundled in Prism), which
pushes directly to Twitch RTMP without touching Orion.

Tables dropped :
- ``stream_destinations`` — multi-RTMP fan-out
- ``stream_metrics`` — time-series MediaMTX polling
- ``streams`` — broadcast session lifecycle + MediaMTX path orchestration

Column dropped :
- ``chat_messages.stream_id`` — orphaned external reference. Chat is
  channel-keyed only ; analytics group by ``channel`` and ``sent_at``.

Enum dropped : ``stream_state`` PostgreSQL enum.

The downgrade reverses the structure but does not restore data — the
streaming feature set is gone for good.
"""

from typing import Sequence, Union

import sqlalchemy as sa
from alembic import op
from sqlalchemy.dialects import postgresql

revision: str = "0006_drop_streaming"
down_revision: Union[str, Sequence[str], None] = "0005_destinations"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    # 1. Detach chat_messages from streams (FK auto-named by SQLAlchemy
    # convention as <table>_<column>_fkey). IF EXISTS guards are a belt
    # against rename drift between environments.
    op.execute(
        "ALTER TABLE chat_messages "
        "DROP CONSTRAINT IF EXISTS chat_messages_stream_id_fkey;"
    )
    op.drop_index(op.f("ix_chat_messages_stream_id"), table_name="chat_messages")
    op.drop_column("chat_messages", "stream_id")

    # 2. Children of `streams` first.
    op.drop_index("ix_stream_destinations_stream_id", table_name="stream_destinations")
    op.drop_table("stream_destinations")

    op.drop_index(op.f("ix_stream_metrics_ts"), table_name="stream_metrics")
    op.drop_index(op.f("ix_stream_metrics_stream_id"), table_name="stream_metrics")
    op.drop_table("stream_metrics")

    # 3. streams itself.
    op.drop_index(op.f("ix_streams_ingress_token"), table_name="streams")
    op.drop_index(op.f("ix_streams_state"), table_name="streams")
    op.drop_index(op.f("ix_streams_credential_id"), table_name="streams")
    op.drop_index(op.f("ix_streams_overlay_id"), table_name="streams")
    op.drop_index(op.f("ix_streams_owner_id"), table_name="streams")
    op.drop_table("streams")

    # 4. PG enum used solely by streams.state.
    sa.Enum(name="stream_state").drop(op.get_bind(), checkfirst=True)


def downgrade() -> None:
    """Reverse the structure (no data restoration — feature is dead)."""
    stream_state = postgresql.ENUM(
        "pending",
        "preparing",
        "live",
        "stopping",
        "ended",
        "error",
        name="stream_state",
    )
    stream_state.create(op.get_bind(), checkfirst=True)

    op.create_table(
        "streams",
        sa.Column("id", postgresql.UUID(as_uuid=True), primary_key=True),
        sa.Column("owner_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("overlay_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column(
            "overlay_playlist",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'[]'::jsonb"),
        ),
        sa.Column("credential_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("state", stream_state, nullable=False, server_default="pending"),
        sa.Column("mediamtx_path", sa.String(255), nullable=False),
        sa.Column("whip_endpoint", sa.String(512), nullable=False),
        sa.Column("ingress_token", sa.String(64), nullable=True),
        sa.Column("ingress_token_expires_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("twitch_stream_id", sa.String(64), nullable=True),
        sa.Column("target_width", sa.Integer(), nullable=False, server_default="1920"),
        sa.Column("target_height", sa.Integer(), nullable=False, server_default="1080"),
        sa.Column("target_fps", sa.Integer(), nullable=False, server_default="30"),
        sa.Column("video_bitrate_kbps", sa.Integer(), nullable=False, server_default="6000"),
        sa.Column("audio_bitrate_kbps", sa.Integer(), nullable=False, server_default="160"),
        sa.Column("keyframe_interval_s", sa.Integer(), nullable=False, server_default="2"),
        sa.Column("encoder_preset", sa.String(32), nullable=False, server_default="veryfast"),
        sa.Column("record", sa.Boolean(), nullable=False, server_default=sa.text("false")),
        sa.Column("started_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("ended_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("error_message", sa.Text(), nullable=True),
        sa.Column(
            "metadata",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.text("now()")),
        sa.Column("updated_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.text("now()")),
        sa.ForeignKeyConstraint(["credential_id"], ["twitch_credentials.id"], ondelete="RESTRICT"),
    )
    op.create_index(op.f("ix_streams_owner_id"), "streams", ["owner_id"], unique=False)
    op.create_index(op.f("ix_streams_overlay_id"), "streams", ["overlay_id"], unique=False)
    op.create_index(op.f("ix_streams_credential_id"), "streams", ["credential_id"], unique=False)
    op.create_index(op.f("ix_streams_state"), "streams", ["state"], unique=False)
    op.create_index(op.f("ix_streams_ingress_token"), "streams", ["ingress_token"], unique=False)

    op.create_table(
        "stream_metrics",
        sa.Column("id", sa.BigInteger(), primary_key=True, autoincrement=True),
        sa.Column("stream_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("ts", sa.DateTime(timezone=True), nullable=False),
        sa.Column("bitrate_kbps", sa.Integer(), nullable=True),
        sa.Column("fps", sa.Integer(), nullable=True),
        sa.Column("dropped_frames", sa.Integer(), nullable=True),
        sa.Column("rtt_ms", sa.Integer(), nullable=True),
        sa.Column("viewers", sa.Integer(), nullable=True),
        sa.Column(
            "raw",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.ForeignKeyConstraint(["stream_id"], ["streams.id"], ondelete="CASCADE"),
    )
    op.create_index(op.f("ix_stream_metrics_stream_id"), "stream_metrics", ["stream_id"], unique=False)
    op.create_index(op.f("ix_stream_metrics_ts"), "stream_metrics", ["ts"], unique=False)

    op.create_table(
        "stream_destinations",
        sa.Column("id", postgresql.UUID(as_uuid=True), primary_key=True),
        sa.Column("stream_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("kind", sa.String(32), nullable=False, server_default="custom-rtmp"),
        sa.Column("display_name", sa.String(128), nullable=False),
        sa.Column("rtmp_url_ciphertext", sa.LargeBinary(), nullable=False),
        sa.Column("rtmp_url_nonce", sa.LargeBinary(), nullable=False),
        sa.Column("stream_key_ciphertext", sa.LargeBinary(), nullable=False),
        sa.Column("stream_key_nonce", sa.LargeBinary(), nullable=False),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.text("true")),
        sa.Column("ordering", sa.Integer(), nullable=False, server_default=sa.text("0")),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.text("now()")),
        sa.Column("updated_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.text("now()")),
        sa.ForeignKeyConstraint(["stream_id"], ["streams.id"], ondelete="CASCADE"),
    )
    op.create_index("ix_stream_destinations_stream_id", "stream_destinations", ["stream_id"], unique=False)

    op.add_column(
        "chat_messages",
        sa.Column("stream_id", postgresql.UUID(as_uuid=True), nullable=True),
    )
    op.create_foreign_key(
        "chat_messages_stream_id_fkey",
        "chat_messages",
        "streams",
        ["stream_id"],
        ["id"],
        ondelete="SET NULL",
    )
    op.create_index(op.f("ix_chat_messages_stream_id"), "chat_messages", ["stream_id"], unique=False)
