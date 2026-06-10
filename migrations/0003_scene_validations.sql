-- +goose Up
-- +goose StatementBegin

-- ADR 003 §3.2.2 (issue #87) — the scene-validation gate. A validation
-- campaign runs against a pushed (scene_id, scene_version) in an isolated
-- validation-mode clone; the record below is what makes a version
-- air-eligible. A version with NO `validated` record for the CURRENT
-- harness_version cannot be activated, push-swapped onto air, or rolled
-- back to (SCENE_NOT_VALIDATED) — authoring is never blocked, only the
-- antenna waits for proof.
--
-- Invalidation is by construction (§3.2.2): the record is keyed by
-- scene_version, which is the artefact content hash — any change mints a
-- new version, which has no record, so it is not air-eligible. No
-- staleness tracking is needed. harness_version additionally allows
-- fleet-wide re-validation when the harness semantics change (a new
-- harness_version means existing records no longer satisfy the gate).
--
-- The composite key (scene_id, scene_version, harness_version) lets a
-- re-validation under a new harness_version coexist with the old record
-- rather than overwrite it (audit trail). The FK to scene_pushed_versions
-- with ON DELETE CASCADE is the archive-purge coherence guarantee
-- (§3.2.2): purging a version's artefacts deletes its validation records
-- in the SAME breath, so a later re-push of byte-identical content
-- re-mints the hash but resurrects NO record — it must re-validate.

CREATE TABLE scene_validations (
    scene_id         uuid        NOT NULL,
    scene_version    text        NOT NULL,
    harness_version  text        NOT NULL,
    status           text        NOT NULL CHECK (status IN ('validated', 'failed')),
    report           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scene_id, scene_version, harness_version),
    FOREIGN KEY (scene_id, scene_version)
        REFERENCES scene_pushed_versions (scene_id, scene_version)
        ON DELETE CASCADE
);

CREATE INDEX idx_scene_validations_scene
    ON scene_validations (scene_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS scene_validations;
-- +goose StatementEnd
