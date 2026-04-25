"""create stream_destinations table

Revision ID: 0005_destinations
Revises: 0004_record
Create Date: 2026-04-26 01:00:00.000000

Multi-output streaming. The legacy `streams.credential_id` Twitch
path stays untouched; new destinations attached via this table fan
out the same transcoded video to additional RTMP endpoints (Twitch
co-streaming, YouTube Live, Facebook Live, custom RTMP) using the
ffmpeg tee muxer.
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "0005_destinations"
down_revision: Union[str, Sequence[str], None] = "0004_record"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    op.create_table(
        "stream_destinations",
        sa.Column("id", sa.dialects.postgresql.UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "stream_id",
            sa.dialects.postgresql.UUID(as_uuid=True),
            sa.ForeignKey("streams.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("kind", sa.String(32), nullable=False, server_default="custom-rtmp"),
        sa.Column("display_name", sa.String(128), nullable=False),
        sa.Column("rtmp_url_ciphertext", sa.LargeBinary(), nullable=False),
        sa.Column("rtmp_url_nonce", sa.LargeBinary(), nullable=False),
        sa.Column("stream_key_ciphertext", sa.LargeBinary(), nullable=False),
        sa.Column("stream_key_nonce", sa.LargeBinary(), nullable=False),
        sa.Column("enabled", sa.Boolean(), nullable=False, server_default=sa.text("true")),
        sa.Column("ordering", sa.Integer(), nullable=False, server_default=sa.text("0")),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()),
        sa.Column("updated_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()),
    )
    op.create_index(
        "ix_stream_destinations_stream_id",
        "stream_destinations",
        ["stream_id"],
    )


def downgrade() -> None:
    op.drop_index("ix_stream_destinations_stream_id", table_name="stream_destinations")
    op.drop_table("stream_destinations")
