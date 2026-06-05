-- +goose Up
-- +goose StatementBegin

-- ADR 007 §C.2 (C2 — Orion serves LSML bytes). Additive, nullable
-- columns beside the bespoke render artefact. A pushed version may now
-- ALSO carry the LSML 1.1 bundle the compiler emits (EmitLSML, C1),
-- content-addressed by its own lsml.HashBundle. Both columns are NULL
-- for versions pushed in `bespoke` mode (the default), so a deploy of
-- this migration without ORION_LSDP_MODE=dual|lsdp is a no-op for the
-- read path — nothing is persisted, nothing is served.
--
-- lsml_bundle_jsonb : the sealed LSML bundle JSON (opaque bytes; Orion
--                     never walks the layout tree — the TS runtime does).
-- lsml_bundle_hash  : the LSML content address ("sha256:<hex>"), the
--                     GET ?v= key. Indexed for the by-hash lookup.

ALTER TABLE scene_pushed_versions
    ADD COLUMN lsml_bundle_jsonb jsonb NULL,
    ADD COLUMN lsml_bundle_hash  text  NULL;

CREATE INDEX idx_scene_pushed_versions_lsml_hash
    ON scene_pushed_versions (scene_id, lsml_bundle_hash)
    WHERE lsml_bundle_hash IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_scene_pushed_versions_lsml_hash;
ALTER TABLE scene_pushed_versions
    DROP COLUMN IF EXISTS lsml_bundle_hash,
    DROP COLUMN IF EXISTS lsml_bundle_jsonb;
-- +goose StatementEnd
