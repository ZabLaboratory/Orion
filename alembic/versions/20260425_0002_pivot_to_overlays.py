"""pivot streams to overlay_id + streaming params, drop scenes/chat_components

Revision ID: 0002_pivot
Revises: 0001_init
Create Date: 2026-04-25 00:00:00.000000

The architectural pivot: Orion stops authoring scenes. Visual composition
moves to ZabCanvas (with Blue blueprints resolved at render time); Orion
keeps only what's intrinsically a streaming concern (credentials, stream
lifecycle, MediaMTX orchestration, Twitch IRC pump).

Concretely:
  - drop ``scenes`` (and the FK ``streams.scene_id`` that pointed at it)
  - drop ``chat_components`` (chat is a Blue blueprint sat on a ZabCanvas
    overlay component; Orion no longer needs a registry)
  - rebuild ``streams``:
      * ``scene_id`` → ``overlay_id`` (UUID, nullable, no FK — soft pointer
        into ZabCanvas)
      * add streaming params: target_width/height/fps,
        video_bitrate_kbps, audio_bitrate_kbps, keyframe_interval_s,
        encoder_preset
  - keep ``chat_messages`` (replay/audit) but its FK to ``streams`` needs
    to be re-pointed at the rebuilt table.

Existing stream / scene / chat_component / chat_message rows are scaffold
data with no production value — the migration drops and recreates rather
than carrying ghost references through.
"""

from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa
from sqlalchemy.dialects import postgresql


revision: str = "0002_pivot"
down_revision: Union[str, Sequence[str], None] = "0001_init"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


STREAM_STATE_VALUES = ("pending", "preparing", "live", "stopping", "ended", "error")


def upgrade() -> None:
    """Upgrade schema."""
    # Drop tables in dependency order so the FKs unwind cleanly.
    op.drop_index(op.f("ix_chat_messages_channel"), table_name="chat_messages")
    op.drop_index(op.f("ix_chat_messages_stream_id"), table_name="chat_messages")
    op.drop_table("chat_messages")

    op.drop_index(op.f("ix_chat_components_blueprint_ref"), table_name="chat_components")
    op.drop_index(op.f("ix_chat_components_owner_id"), table_name="chat_components")
    op.drop_table("chat_components")

    # stream_metrics has FK to streams — drop it first too (cascade-careful).
    op.drop_index(op.f("ix_stream_metrics_ts"), table_name="stream_metrics")
    op.drop_index(op.f("ix_stream_metrics_stream_id"), table_name="stream_metrics")
    op.drop_table("stream_metrics")

    op.drop_index(op.f("ix_streams_ingress_token"), table_name="streams")
    op.drop_index(op.f("ix_streams_state"), table_name="streams")
    op.drop_index(op.f("ix_streams_credential_id"), table_name="streams")
    op.drop_index(op.f("ix_streams_scene_id"), table_name="streams")
    op.drop_index(op.f("ix_streams_owner_id"), table_name="streams")
    op.drop_table("streams")

    op.drop_index(op.f("ix_scenes_owner_id"), table_name="scenes")
    op.drop_table("scenes")

    # Recreate streams with overlay_id + streaming params.
    op.create_table(
        "streams",
        sa.Column("id", postgresql.UUID(as_uuid=True), nullable=False, server_default=sa.text("gen_random_uuid()")),
        sa.Column("owner_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("overlay_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("credential_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column(
            "state",
            postgresql.ENUM(*STREAM_STATE_VALUES, name="stream_state", create_type=False),
            nullable=False,
            server_default=sa.text("'pending'::stream_state"),
        ),
        sa.Column("mediamtx_path", sa.String(length=128), nullable=False),
        sa.Column("whip_endpoint", sa.String(length=1024), nullable=False),
        sa.Column("ingress_token", sa.String(length=128), nullable=False),
        sa.Column("ingress_token_expires_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("twitch_stream_id", sa.String(length=64), nullable=True),
        sa.Column("target_width", sa.Integer(), nullable=False, server_default=sa.text("1920")),
        sa.Column("target_height", sa.Integer(), nullable=False, server_default=sa.text("1080")),
        sa.Column("target_fps", sa.Integer(), nullable=False, server_default=sa.text("30")),
        sa.Column("video_bitrate_kbps", sa.Integer(), nullable=False, server_default=sa.text("6000")),
        sa.Column("audio_bitrate_kbps", sa.Integer(), nullable=False, server_default=sa.text("160")),
        sa.Column("keyframe_interval_s", sa.Integer(), nullable=False, server_default=sa.text("2")),
        sa.Column("encoder_preset", sa.String(length=32), nullable=False, server_default=sa.text("'veryfast'")),
        sa.Column("started_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("ended_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("error_message", sa.String(length=1024), nullable=True),
        sa.Column(
            "metadata",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.ForeignKeyConstraint(["credential_id"], ["twitch_credentials.id"], ondelete="RESTRICT"),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("mediamtx_path", name="uq_streams_mediamtx_path"),
    )
    op.create_index(op.f("ix_streams_owner_id"), "streams", ["owner_id"], unique=False)
    op.create_index(op.f("ix_streams_overlay_id"), "streams", ["overlay_id"], unique=False)
    op.create_index(op.f("ix_streams_credential_id"), "streams", ["credential_id"], unique=False)
    op.create_index(op.f("ix_streams_state"), "streams", ["state"], unique=False)
    op.create_index(op.f("ix_streams_ingress_token"), "streams", ["ingress_token"], unique=False)

    # Recreate chat_messages with FK to the rebuilt streams table.
    op.create_table(
        "chat_messages",
        sa.Column("id", sa.BigInteger(), autoincrement=True, nullable=False),
        sa.Column("stream_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("channel", sa.String(length=255), nullable=False),
        sa.Column("author_login", sa.String(length=255), nullable=False),
        sa.Column("author_id", sa.String(length=64), nullable=True),
        sa.Column("author_display", sa.String(length=255), nullable=True),
        sa.Column("content", sa.Text(), nullable=False),
        sa.Column(
            "badges",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column(
            "emotes",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column("sent_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.ForeignKeyConstraint(["stream_id"], ["streams.id"], ondelete="SET NULL"),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index(op.f("ix_chat_messages_stream_id"), "chat_messages", ["stream_id"], unique=False)
    op.create_index(op.f("ix_chat_messages_channel"), "chat_messages", ["channel"], unique=False)

    # Recreate stream_metrics — same shape, FK to the new streams table.
    op.create_table(
        "stream_metrics",
        sa.Column("id", sa.BigInteger(), autoincrement=True, nullable=False),
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
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index(op.f("ix_stream_metrics_stream_id"), "stream_metrics", ["stream_id"], unique=False)
    op.create_index(op.f("ix_stream_metrics_ts"), "stream_metrics", ["ts"], unique=False)


def downgrade() -> None:
    """Downgrade schema — restore the v0.1.0 layout (lossy: data is dropped)."""
    op.drop_index(op.f("ix_stream_metrics_ts"), table_name="stream_metrics")
    op.drop_index(op.f("ix_stream_metrics_stream_id"), table_name="stream_metrics")
    op.drop_table("stream_metrics")

    op.drop_index(op.f("ix_chat_messages_channel"), table_name="chat_messages")
    op.drop_index(op.f("ix_chat_messages_stream_id"), table_name="chat_messages")
    op.drop_table("chat_messages")

    op.drop_index(op.f("ix_streams_ingress_token"), table_name="streams")
    op.drop_index(op.f("ix_streams_state"), table_name="streams")
    op.drop_index(op.f("ix_streams_credential_id"), table_name="streams")
    op.drop_index(op.f("ix_streams_overlay_id"), table_name="streams")
    op.drop_index(op.f("ix_streams_owner_id"), table_name="streams")
    op.drop_table("streams")

    op.create_table(
        "scenes",
        sa.Column("id", postgresql.UUID(as_uuid=True), nullable=False, server_default=sa.text("gen_random_uuid()")),
        sa.Column("owner_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("name", sa.String(length=255), nullable=False),
        sa.Column("description", sa.String(length=1024), nullable=True),
        sa.Column("width", sa.Integer(), nullable=False, server_default=sa.text("1920")),
        sa.Column("height", sa.Integer(), nullable=False, server_default=sa.text("1080")),
        sa.Column("fps", sa.Integer(), nullable=False, server_default=sa.text("30")),
        sa.Column(
            "config",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column("thumbnail", sa.String(length=4096), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index(op.f("ix_scenes_owner_id"), "scenes", ["owner_id"], unique=False)

    op.create_table(
        "streams",
        sa.Column("id", postgresql.UUID(as_uuid=True), nullable=False, server_default=sa.text("gen_random_uuid()")),
        sa.Column("owner_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("scene_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("credential_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column(
            "state",
            postgresql.ENUM(*STREAM_STATE_VALUES, name="stream_state", create_type=False),
            nullable=False,
            server_default=sa.text("'pending'::stream_state"),
        ),
        sa.Column("mediamtx_path", sa.String(length=128), nullable=False),
        sa.Column("whip_endpoint", sa.String(length=1024), nullable=False),
        sa.Column("ingress_token", sa.String(length=128), nullable=False),
        sa.Column("ingress_token_expires_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("twitch_stream_id", sa.String(length=64), nullable=True),
        sa.Column("started_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("ended_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("error_message", sa.String(length=1024), nullable=True),
        sa.Column(
            "metadata",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.ForeignKeyConstraint(["scene_id"], ["scenes.id"], ondelete="RESTRICT"),
        sa.ForeignKeyConstraint(["credential_id"], ["twitch_credentials.id"], ondelete="RESTRICT"),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("mediamtx_path", name="uq_streams_mediamtx_path"),
    )
    op.create_index(op.f("ix_streams_owner_id"), "streams", ["owner_id"], unique=False)
    op.create_index(op.f("ix_streams_scene_id"), "streams", ["scene_id"], unique=False)
    op.create_index(op.f("ix_streams_credential_id"), "streams", ["credential_id"], unique=False)
    op.create_index(op.f("ix_streams_state"), "streams", ["state"], unique=False)
    op.create_index(op.f("ix_streams_ingress_token"), "streams", ["ingress_token"], unique=False)

    op.create_table(
        "chat_components",
        sa.Column("id", postgresql.UUID(as_uuid=True), nullable=False, server_default=sa.text("gen_random_uuid()")),
        sa.Column("owner_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("name", sa.String(length=255), nullable=False),
        sa.Column("blueprint_ref", sa.String(length=255), nullable=False),
        sa.Column(
            "config", postgresql.JSONB(astext_type=sa.Text()), nullable=False, server_default=sa.text("'{}'::jsonb")
        ),
        sa.Column(
            "placement", postgresql.JSONB(astext_type=sa.Text()), nullable=False, server_default=sa.text("'{}'::jsonb")
        ),
        sa.Column(
            "triggers", postgresql.JSONB(astext_type=sa.Text()), nullable=False, server_default=sa.text("'{}'::jsonb")
        ),
        sa.Column("is_enabled", sa.Boolean(), nullable=False, server_default=sa.text("true")),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index(op.f("ix_chat_components_owner_id"), "chat_components", ["owner_id"], unique=False)
    op.create_index(op.f("ix_chat_components_blueprint_ref"), "chat_components", ["blueprint_ref"], unique=False)

    op.create_table(
        "chat_messages",
        sa.Column("id", sa.BigInteger(), autoincrement=True, nullable=False),
        sa.Column("stream_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("channel", sa.String(length=255), nullable=False),
        sa.Column("author_login", sa.String(length=255), nullable=False),
        sa.Column("author_id", sa.String(length=64), nullable=True),
        sa.Column("author_display", sa.String(length=255), nullable=True),
        sa.Column("content", sa.Text(), nullable=False),
        sa.Column(
            "badges", postgresql.JSONB(astext_type=sa.Text()), nullable=False, server_default=sa.text("'{}'::jsonb")
        ),
        sa.Column(
            "emotes", postgresql.JSONB(astext_type=sa.Text()), nullable=False, server_default=sa.text("'{}'::jsonb")
        ),
        sa.Column("sent_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.ForeignKeyConstraint(["stream_id"], ["streams.id"], ondelete="SET NULL"),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index(op.f("ix_chat_messages_stream_id"), "chat_messages", ["stream_id"], unique=False)
    op.create_index(op.f("ix_chat_messages_channel"), "chat_messages", ["channel"], unique=False)

    op.create_table(
        "stream_metrics",
        sa.Column("id", sa.BigInteger(), autoincrement=True, nullable=False),
        sa.Column("stream_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("ts", sa.DateTime(timezone=True), nullable=False),
        sa.Column("bitrate_kbps", sa.Integer(), nullable=True),
        sa.Column("fps", sa.Integer(), nullable=True),
        sa.Column("dropped_frames", sa.Integer(), nullable=True),
        sa.Column("rtt_ms", sa.Integer(), nullable=True),
        sa.Column("viewers", sa.Integer(), nullable=True),
        sa.Column(
            "raw", postgresql.JSONB(astext_type=sa.Text()), nullable=False, server_default=sa.text("'{}'::jsonb")
        ),
        sa.ForeignKeyConstraint(["stream_id"], ["streams.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index(op.f("ix_stream_metrics_stream_id"), "stream_metrics", ["stream_id"], unique=False)
    op.create_index(op.f("ix_stream_metrics_ts"), "stream_metrics", ["ts"], unique=False)
