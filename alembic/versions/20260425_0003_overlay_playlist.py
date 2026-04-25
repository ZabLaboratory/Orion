"""add overlay_playlist column to streams

Revision ID: 0003_playlist
Revises: 0002_pivot
Create Date: 2026-04-25 18:00:00.000000

Phase 4 (scene-switcher): the operator pre-declares a list of overlays
the stream can swap to live. ``overlay_id`` (the active overlay) must
belong to this playlist; switching mid-stream goes through
``POST /streams/{id}/active-overlay`` which validates membership and
fans out a state event to subscribers.
"""
from typing import Sequence, Union

from alembic import op
import sqlalchemy as sa
from sqlalchemy.dialects import postgresql


# revision identifiers, used by Alembic.
revision: str = "0003_playlist"
down_revision: Union[str, Sequence[str], None] = "0002_pivot"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    op.add_column(
        "streams",
        sa.Column(
            "overlay_playlist",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'[]'::jsonb"),
        ),
    )


def downgrade() -> None:
    op.drop_column("streams", "overlay_playlist")
