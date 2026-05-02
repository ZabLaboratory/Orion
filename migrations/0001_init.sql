-- +goose Up
-- +goose StatementBegin

-- Per ADR 004 § 10:
--   - definitions are kept forever (audit trail, source of recompiles)
--   - pushed versions are immutable compiled artifacts, retained while
--     the parent scene is `active` (purged on archive, § 10.1)
--   - assets are content-addressed; the binary lives on the filesystem,
--     this table holds metadata only

CREATE TABLE scenes (
    id                       uuid PRIMARY KEY,
    name                     text        NOT NULL,
    status                   text        NOT NULL DEFAULT 'active'
                                         CHECK (status IN ('active', 'archived')),
    latest_pushed_version    text                 NULL,  -- nullable until first push
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_scenes_status ON scenes (status);

-- Every save produces a row. Definitions are append-only; mutations
-- come as new rows with bumped definition_version.
CREATE TABLE scene_definitions (
    id                   uuid        PRIMARY KEY,
    scene_id             uuid        NOT NULL REFERENCES scenes(id) ON DELETE CASCADE,
    definition_version   integer     NOT NULL,
    canvas_version       text        NOT NULL,
    blue_blueprint_id    text        NOT NULL,
    components_jsonb     jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scene_id, definition_version)
);

CREATE INDEX idx_scene_definitions_scene ON scene_definitions (scene_id, definition_version DESC);

-- Pushed versions are the only artifacts the runtime ever reads.
-- Composite PK = (scene_id, scene_version): scene_version is the
-- content hash so the same hash should never collide across scenes.
CREATE TABLE scene_pushed_versions (
    scene_id            uuid        NOT NULL REFERENCES scenes(id) ON DELETE CASCADE,
    scene_version       text        NOT NULL,
    definition_id       uuid        NOT NULL REFERENCES scene_definitions(id) ON DELETE RESTRICT,
    graph_jsonb         jsonb       NOT NULL,
    bundle_jsonb        jsonb       NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scene_id, scene_version)
);

CREATE INDEX idx_scene_pushed_versions_scene ON scene_pushed_versions (scene_id, created_at DESC);

-- Assets are content-addressed; the row keeps mime + size + filesystem
-- path so the API can serve them without re-hashing on every request.
CREATE TABLE assets (
    id                  uuid        PRIMARY KEY,
    sha256_hex          text        NOT NULL UNIQUE,
    mime                text        NOT NULL,
    size_bytes          bigint      NOT NULL CHECK (size_bytes >= 0),
    filesystem_path     text        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS assets;
DROP TABLE IF EXISTS scene_pushed_versions;
DROP TABLE IF EXISTS scene_definitions;
DROP TABLE IF EXISTS scenes;
-- +goose StatementEnd
