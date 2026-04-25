"""add record flag to streams

Revision ID: 0004_record
Revises: 0003_playlist
Create Date: 2026-04-26 00:30:00.000000

Recording / DVR feature — when true, the ffmpeg child writes a second
MP4 output to /recordings/<stream_id>/ via the tee muxer alongside the
RTMP push to Twitch. Flag is consumed at start_stream time; toggling
mid-stream has no effect until the next start.
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa


# revision identifiers, used by Alembic.
revision: str = "0004_record"
down_revision: Union[str, Sequence[str], None] = "0003_playlist"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    op.add_column(
        "streams",
        sa.Column(
            "record",
            sa.Boolean(),
            nullable=False,
            server_default=sa.text("false"),
        ),
    )


def downgrade() -> None:
    op.drop_column("streams", "record")
