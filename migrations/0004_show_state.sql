-- +goose Up
-- +goose StatementBegin

-- The live antenna pointer. The scene SELECTION (which scene is on air) is
-- persisted broadcast config — it must survive a restart/redeploy, or the
-- antenna comes back dark after every deploy and every viewer's WS is closed
-- with `scene not found` until an operator re-pushes (the workaround this
-- migration retires). This is distinct from `scenes.status='active'` (= "in
-- the roster, not archived") and from `latest_pushed_version` (= "which
-- compiled artefact"): here we record WHICH active+pushed scene holds the
-- antenna.
--
-- Note the separation of concerns w.r.t. criterion #11 (ADR 004 §12): leaf
-- VALUES are never persisted — they reseed from declared defaults on every
-- boot (State.Seed). Only the scene SELECTION is durable here. Live state
-- stays volatile; the antenna choice is config.
--
-- Singleton row: id is a boolean PK pinned to TRUE, so there is exactly one
-- show_state row. active_scene_id is nullable (no scene on air yet) and FKs
-- to scenes with ON DELETE SET NULL, so archiving/deleting the on-air scene
-- clears the pointer rather than dangling it.

CREATE TABLE show_state (
    id              boolean     PRIMARY KEY DEFAULT TRUE CHECK (id = TRUE),
    active_scene_id uuid                 NULL REFERENCES scenes(id) ON DELETE SET NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Seed the singleton row so callers UPDATE a row that always exists.
INSERT INTO show_state (id, active_scene_id) VALUES (TRUE, NULL);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS show_state;
-- +goose StatementEnd
