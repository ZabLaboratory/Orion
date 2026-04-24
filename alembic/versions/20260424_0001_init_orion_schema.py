"""init Orion schema

Revision ID: 0001_init
Revises:
Create Date: 2026-04-24 12:00:00.000000

"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa
from sqlalchemy.dialects import postgresql


# revision identifiers, used by Alembic.
revision: str = "0001_init"
down_revision: Union[str, Sequence[str], None] = None
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


STREAM_STATE_VALUES = ("pending", "preparing", "live", "stopping", "ended", "error")


def upgrade() -> None:
    """Upgrade schema."""
    # Postgres gen_random_uuid() lives in pgcrypto (Postgres 13+).
    op.execute('CREATE EXTENSION IF NOT EXISTS "pgcrypto"')

    stream_state = postgresql.ENUM(*STREAM_STATE_VALUES, name="stream_state")
    stream_state.create(op.get_bind(), checkfirst=True)

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
        "twitch_credentials",
        sa.Column("id", postgresql.UUID(as_uuid=True), nullable=False, server_default=sa.text("gen_random_uuid()")),
        sa.Column("owner_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("label", sa.String(length=255), nullable=False),
        sa.Column("channel_login", sa.String(length=255), nullable=True),
        sa.Column("channel_id", sa.String(length=64), nullable=True),
        sa.Column("stream_key_ciphertext", sa.LargeBinary(), nullable=False),
        sa.Column("stream_key_nonce", sa.LargeBinary(), nullable=False),
        sa.Column("oauth_access_ciphertext", sa.LargeBinary(), nullable=True),
        sa.Column("oauth_access_nonce", sa.LargeBinary(), nullable=True),
        sa.Column("oauth_refresh_ciphertext", sa.LargeBinary(), nullable=True),
        sa.Column("oauth_refresh_nonce", sa.LargeBinary(), nullable=True),
        sa.Column("oauth_expires_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("oauth_scopes", postgresql.ARRAY(sa.String(length=64)), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.Column("updated_at", sa.DateTime(timezone=True), server_default=sa.text("now()"), nullable=False),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index(op.f("ix_twitch_credentials_owner_id"), "twitch_credentials", ["owner_id"], unique=False)

    op.create_table(
        "streams",
        sa.Column("id", postgresql.UUID(as_uuid=True), nullable=False, server_default=sa.text("gen_random_uuid()")),
        sa.Column("owner_id", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("scene_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("credential_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column(
            "state",
            sa.Enum(*STREAM_STATE_VALUES, name="stream_state", create_type=False),
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
            "config",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column(
            "placement",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column(
            "triggers",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
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
    """Downgrade schema."""
    op.drop_index(op.f("ix_stream_metrics_ts"), table_name="stream_metrics")
    op.drop_index(op.f("ix_stream_metrics_stream_id"), table_name="stream_metrics")
    op.drop_table("stream_metrics")

    op.drop_index(op.f("ix_chat_messages_channel"), table_name="chat_messages")
    op.drop_index(op.f("ix_chat_messages_stream_id"), table_name="chat_messages")
    op.drop_table("chat_messages")

    op.drop_index(op.f("ix_chat_components_blueprint_ref"), table_name="chat_components")
    op.drop_index(op.f("ix_chat_components_owner_id"), table_name="chat_components")
    op.drop_table("chat_components")

    op.drop_index(op.f("ix_streams_ingress_token"), table_name="streams")
    op.drop_index(op.f("ix_streams_state"), table_name="streams")
    op.drop_index(op.f("ix_streams_credential_id"), table_name="streams")
    op.drop_index(op.f("ix_streams_scene_id"), table_name="streams")
    op.drop_index(op.f("ix_streams_owner_id"), table_name="streams")
    op.drop_table("streams")

    op.drop_index(op.f("ix_twitch_credentials_owner_id"), table_name="twitch_credentials")
    op.drop_table("twitch_credentials")

    op.drop_index(op.f("ix_scenes_owner_id"), table_name="scenes")
    op.drop_table("scenes")

    stream_state = postgresql.ENUM(*STREAM_STATE_VALUES, name="stream_state")
    stream_state.drop(op.get_bind(), checkfirst=True)
