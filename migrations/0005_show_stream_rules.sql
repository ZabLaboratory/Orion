-- +goose Up
-- +goose StatementBegin

-- Stream-level Blue rules (ADR 009 §3.1/§3.5, issue #154). A promoted
-- stream rule is a roster scene that the operator has elected to run
-- ALWAYS — never gated on the active-scene pointer, never frozen at a
-- scene switch (the routing union + ungated lifecycle land in #153/#156).
--
-- This table persists the SET of promoted rule ids so the selection
-- survives a restart/redeploy, exactly as show_state (0004) persists the
-- active-scene SELECTION. On boot, cmd/orion reseeds each promoted rule
-- into the roster (PromoteStreamRule) — criterion #11 holds the same way
-- it does for the active pointer: only the SELECTION is durable, never any
-- live leaf state (a rule reseeds from declared defaults and fires its
-- on-start once on reload, ADR 009 §3.4).
--
-- One row per promoted scene. scene_id FKs to scenes with ON DELETE
-- CASCADE: archiving/deleting a scene drops its rule membership (a deleted
-- scene cannot be a rule). The API additionally refuses to ARCHIVE a
-- currently-promoted scene (SCENE_IN_USE), so a cascade only fires on a
-- hard scene deletion, never on the normal archive path.
CREATE TABLE show_stream_rules (
    scene_id    uuid        PRIMARY KEY REFERENCES scenes(id) ON DELETE CASCADE,
    promoted_at timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS show_stream_rules;
-- +goose StatementEnd
