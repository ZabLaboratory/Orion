package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SceneValidation is one row of scene_validations (ADR 003 §3.2.2, issue
// #87): the record that makes a (scene_id, scene_version) air-eligible for
// a given harness_version. Keyed by (scene_id, scene_version,
// harness_version); the FK to scene_pushed_versions with ON DELETE CASCADE
// is the archive-purge coherence guarantee.
//
// The json tags are the embedded-local validation-mirror wire contract
// (Conduit A1): the ZabCanvas export (#145) writes the seed in snake_case
// and MirrorValidator (#247) reads it back into this struct, so the tags
// must match the producer's casing or the unmarshal silently yields a
// zero-value record and the gate refuses every scene. The DB path does not
// use these tags (pgx scans columns), so adding them is inert for antenne.
type SceneValidation struct {
	SceneID        uuid.UUID       `json:"scene_id"`
	SceneVersion   string          `json:"scene_version"`
	HarnessVersion string          `json:"harness_version"`
	Status         string          `json:"status"` // "validated" | "failed"
	Report         json.RawMessage `json:"report"`
	CreatedAt      time.Time       `json:"created_at,omitempty"`
}

// ValidationValidated is the status that makes a version air-eligible.
const ValidationValidated = "validated"

// UpsertValidation records (or overwrites) a campaign result. Re-running a
// campaign for the same (scene, version, harness) replaces the prior
// record — the latest campaign is authoritative. Pool-scoped: a campaign
// is the single writer for its (scene, version, harness) triple.
func (s *PGStore) UpsertValidation(ctx context.Context, v SceneValidation) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO scene_validations
		   (scene_id, scene_version, harness_version, status, report, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (scene_id, scene_version, harness_version)
		 DO UPDATE SET status = EXCLUDED.status,
		               report = EXCLUDED.report,
		               created_at = EXCLUDED.created_at`,
		v.SceneID, v.SceneVersion, v.HarnessVersion, v.Status, v.Report, v.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert validation: %w", err)
	}
	return nil
}

// GetValidation fetches the validation record for a specific
// (scene, version, harness). ErrNotFound when no campaign has run for that
// triple — which the gate treats as NOT air-eligible (no record ⇒ refuse).
func (s *PGStore) GetValidation(ctx context.Context, sceneID uuid.UUID, sceneVersion, harnessVersion string) (*SceneValidation, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT scene_id, scene_version, harness_version, status, report, created_at
		   FROM scene_validations
		  WHERE scene_id = $1 AND scene_version = $2 AND harness_version = $3`,
		sceneID, sceneVersion, harnessVersion,
	)
	var v SceneValidation
	err := row.Scan(&v.SceneID, &v.SceneVersion, &v.HarnessVersion, &v.Status, &v.Report, &v.CreatedAt)
	if err != nil {
		return nil, noRow(err)
	}
	return &v, nil
}

// IsVersionValidated reports whether a (scene, version) carries a
// `validated` record for harnessVersion — the gate's air-eligibility test
// (§3.2.2). A missing record (ErrNotFound) is NOT validated, not an error:
// the gate refuses, the campaign mints the record. Any other error
// propagates (the caller fails closed on a DB error rather than airing an
// unproven version).
func (s *PGStore) IsVersionValidated(ctx context.Context, sceneID uuid.UUID, sceneVersion, harnessVersion string) (bool, error) {
	v, err := s.GetValidation(ctx, sceneID, sceneVersion, harnessVersion)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v.Status == ValidationValidated, nil
}
