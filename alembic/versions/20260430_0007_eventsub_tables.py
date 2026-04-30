"""eventsub subscriptions + event audit log

Revision ID: 0007_eventsub
Revises: 0006_drop_streaming
Create Date: 2026-04-30 01:00:00.000000

Adds the two tables backing the WebSocket-transport EventSub :

- ``eventsub_subscriptions`` mirrors the Twitch-side subscription list
  so we don't query Helix on every UI render. CASCADE on
  ``twitch_credentials`` because revoking a credential takes its
  subscriptions with it.
- ``eventsub_events`` audit log + dedup, keyed by ``twitch_message_id``
  (unique). Subscription FK is SET NULL : retain history even after
  Twitch revokes a subscription.
"""

from typing import Sequence, Union

import sqlalchemy as sa
from alembic import op
from sqlalchemy.dialects import postgresql

revision: str = "0007_eventsub"
down_revision: Union[str, Sequence[str], None] = "0006_drop_streaming"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    op.create_table(
        "eventsub_subscriptions",
        sa.Column("id", sa.Uuid(), primary_key=True, server_default=sa.func.gen_random_uuid()),
        sa.Column("credential_id", sa.Uuid(), nullable=False),
        sa.Column("twitch_subscription_id", sa.String(64), nullable=False),
        sa.Column("event_type", sa.String(64), nullable=False),
        sa.Column("version", sa.String(8), nullable=False, server_default="1"),
        sa.Column("status", sa.String(64), nullable=False, server_default="enabled"),
        sa.Column("cost", sa.Integer(), nullable=False, server_default="1"),
        sa.Column(
            "condition",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()),
        sa.Column("updated_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()),
        sa.ForeignKeyConstraint(["credential_id"], ["twitch_credentials.id"], ondelete="CASCADE"),
    )
    op.create_index(
        op.f("ix_eventsub_subscriptions_credential_id"),
        "eventsub_subscriptions",
        ["credential_id"],
        unique=False,
    )
    op.create_index(
        op.f("ix_eventsub_subscriptions_event_type"),
        "eventsub_subscriptions",
        ["event_type"],
        unique=False,
    )
    op.create_index(
        op.f("ix_eventsub_subscriptions_twitch_subscription_id"),
        "eventsub_subscriptions",
        ["twitch_subscription_id"],
        unique=True,
    )

    op.create_table(
        "eventsub_events",
        sa.Column("id", sa.BigInteger(), primary_key=True, autoincrement=True),
        sa.Column("twitch_message_id", sa.String(128), nullable=False),
        sa.Column("subscription_id", sa.Uuid(), nullable=True),
        sa.Column("event_type", sa.String(64), nullable=False),
        sa.Column(
            "event",
            postgresql.JSONB(astext_type=sa.Text()),
            nullable=False,
            server_default=sa.text("'{}'::jsonb"),
        ),
        sa.Column("received_at", sa.DateTime(timezone=True), nullable=False),
        sa.ForeignKeyConstraint(
            ["subscription_id"], ["eventsub_subscriptions.id"], ondelete="SET NULL"
        ),
    )
    op.create_index(
        op.f("ix_eventsub_events_twitch_message_id"),
        "eventsub_events",
        ["twitch_message_id"],
        unique=True,
    )
    op.create_index(
        op.f("ix_eventsub_events_subscription_id"),
        "eventsub_events",
        ["subscription_id"],
        unique=False,
    )
    op.create_index(
        op.f("ix_eventsub_events_event_type"),
        "eventsub_events",
        ["event_type"],
        unique=False,
    )


def downgrade() -> None:
    op.drop_index(op.f("ix_eventsub_events_event_type"), table_name="eventsub_events")
    op.drop_index(op.f("ix_eventsub_events_subscription_id"), table_name="eventsub_events")
    op.drop_index(op.f("ix_eventsub_events_twitch_message_id"), table_name="eventsub_events")
    op.drop_table("eventsub_events")
    op.drop_index(
        op.f("ix_eventsub_subscriptions_twitch_subscription_id"),
        table_name="eventsub_subscriptions",
    )
    op.drop_index(
        op.f("ix_eventsub_subscriptions_event_type"),
        table_name="eventsub_subscriptions",
    )
    op.drop_index(
        op.f("ix_eventsub_subscriptions_credential_id"),
        table_name="eventsub_subscriptions",
    )
    op.drop_table("eventsub_subscriptions")
