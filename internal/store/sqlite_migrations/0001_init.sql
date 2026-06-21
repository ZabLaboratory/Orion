-- SQLite dialect mirror of migrations/0001_init.sql (ADR 016 §3.2 / RC-3).
-- The store keeps every artefact (graph/bundle/components/lsml) as OPAQUE
-- TEXT — no jsonb predicate ever runs on the hot path, so jsonb collapses
-- to TEXT with zero behaviour change. uuid → TEXT (canonical lowercase
-- form), timestamptz → TEXT (RFC3339), bigint → INTEGER. ON DELETE clauses
-- require PRAGMA foreign_keys = ON (set at every connection by the store).

CREATE TABLE scenes (
    id                       TEXT PRIMARY KEY,
    name                     TEXT    NOT NULL,
    status                   TEXT    NOT NULL DEFAULT 'active'
                                     CHECK (status IN ('active', 'archived')),
    latest_pushed_version    TEXT        NULL,
    created_at               TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at               TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX idx_scenes_status ON scenes (status);

CREATE TABLE scene_definitions (
    id                   TEXT    PRIMARY KEY,
    scene_id             TEXT    NOT NULL REFERENCES scenes(id) ON DELETE CASCADE,
    definition_version   INTEGER NOT NULL,
    canvas_version       TEXT    NOT NULL,
    blue_blueprint_id    TEXT    NOT NULL,
    components_jsonb     TEXT    NOT NULL DEFAULT '[]',
    created_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (scene_id, definition_version)
);

CREATE INDEX idx_scene_definitions_scene ON scene_definitions (scene_id, definition_version DESC);

CREATE TABLE scene_pushed_versions (
    scene_id            TEXT    NOT NULL REFERENCES scenes(id) ON DELETE CASCADE,
    scene_version       TEXT    NOT NULL,
    definition_id       TEXT    NOT NULL REFERENCES scene_definitions(id) ON DELETE RESTRICT,
    graph_jsonb         TEXT    NOT NULL,
    bundle_jsonb        TEXT    NOT NULL,
    lsml_bundle_jsonb   TEXT        NULL,
    lsml_bundle_hash    TEXT        NULL,
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (scene_id, scene_version)
);

CREATE INDEX idx_scene_pushed_versions_scene ON scene_pushed_versions (scene_id, created_at DESC);

CREATE INDEX idx_scene_pushed_versions_lsml_hash
    ON scene_pushed_versions (scene_id, lsml_bundle_hash)
    WHERE lsml_bundle_hash IS NOT NULL;

CREATE TABLE assets (
    id                  TEXT    PRIMARY KEY,
    sha256_hex          TEXT    NOT NULL UNIQUE,
    mime                TEXT    NOT NULL,
    size_bytes          INTEGER NOT NULL CHECK (size_bytes >= 0),
    filesystem_path     TEXT    NOT NULL,
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE scene_validations (
    scene_id         TEXT NOT NULL,
    scene_version    TEXT NOT NULL,
    harness_version  TEXT NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('validated', 'failed')),
    report           TEXT NOT NULL DEFAULT '{}',
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (scene_id, scene_version, harness_version),
    FOREIGN KEY (scene_id, scene_version)
        REFERENCES scene_pushed_versions (scene_id, scene_version)
        ON DELETE CASCADE
);

CREATE INDEX idx_scene_validations_scene
    ON scene_validations (scene_id, created_at DESC);

-- Singleton live-antenna pointer (0004 pg mirror). boolean→INTEGER pinned to 1.
CREATE TABLE show_state (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    active_scene_id TEXT        NULL REFERENCES scenes(id) ON DELETE SET NULL,
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

INSERT INTO show_state (id, active_scene_id) VALUES (1, NULL);

-- Stream-level Blue rule set (0005 pg mirror).
CREATE TABLE show_stream_rules (
    scene_id    TEXT PRIMARY KEY REFERENCES scenes(id) ON DELETE CASCADE,
    promoted_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
