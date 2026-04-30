"""Static schema declaration for Orion's blueprint-facing surface.

Read-only per ADR 001 §1. Credential + OAuth authoring still goes
through the dedicated CRUD routes — keeping all crypto behind
``services/encryption.py``.

Security model — column-level filtering by absence, applied
aggressively because Orion holds AES-GCM ciphertext (Twitch stream
keys, OAuth tokens) :

- ``twitch_credentials`` is **omitted entirely** — every interesting
  column is sensitive (ciphertext / nonce / scopes) and the blueprint
  surface has no legitimate read use case yet. When one arises we'll
  expose a derived "summary" view (label, channel_login, has_oauth) ;
  until then default-deny.
- ``chat_messages`` carries no secrets and is exposed in full.
"""

from __future__ import annotations

from queryme import (
    ColumnDef,
    SchemaDescriptor,
    TableDef,
)

CATALOG: SchemaDescriptor = SchemaDescriptor(
    service="orion",
    tables=[
        TableDef(
            name="chat_messages",
            writable=False,
            description="Captured Twitch IRC messages for replay / audit.",
            columns=[
                ColumnDef(name="id", type="integer", primary=True),
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
        ),
        # twitch_credentials is intentionally not in this catalog — see
        # module docstring. All its columns are sensitive ciphertext or
        # secret metadata that has no legitimate generic-read use case.
    ],
)
