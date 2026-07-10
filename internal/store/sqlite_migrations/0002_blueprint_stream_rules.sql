-- Durable set of blueprint-direct stream-level Blue rules (0006 pg mirror,
-- ADR 009 Amendment 1, issue #287). No FK: a blueprint has no scenes row (it
-- lives in Blue). Identity only — live leaf state always reseeds from defaults.
CREATE TABLE show_blueprint_stream_rules (
    blueprint_id TEXT PRIMARY KEY,
    promoted_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
