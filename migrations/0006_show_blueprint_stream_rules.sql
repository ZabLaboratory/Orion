-- +goose Up
-- +goose StatementBegin

-- Durable set of BLUEPRINT-DIRECT stream-level Blue rules (ADR 009 Amendment 1,
-- issue #287). A blueprint-direct rule is promoted straight from a Blue
-- blueprint with no carrier scene (POST /show/stream-rules {blueprint_id}); it
-- is keyed by blueprint_id and compiled in-body from Blue (empty key, empty
-- bundle — a rule runs exec, never renders).
--
-- It CANNOT live in show_stream_rules: that table's key FKs to scenes(id) with
-- ON DELETE CASCADE, and a blueprint has no scenes row (the blueprint lives in
-- Blue, an external service). So this is a separate table with NO FK — Orion is
-- not the authority for a blueprint's existence; a promoted-but-since-deleted
-- blueprint simply fails its boot re-fetch and is skipped (fail-soft).
--
-- Durability is IDENTITY only, exactly like show_stream_rules: only WHICH
-- blueprints are promoted is persisted. A rule's live leaf state always reseeds
-- from declared defaults on reload (criterion #11, ADR 009 §3.4) — nothing of
-- the running rule's runtime state is stored here.
CREATE TABLE show_blueprint_stream_rules (
    blueprint_id uuid        PRIMARY KEY,
    promoted_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS show_blueprint_stream_rules;
-- +goose StatementEnd
