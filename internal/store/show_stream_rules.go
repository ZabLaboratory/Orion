package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// The persistence of the stream-level Blue rule set (ADR 009 §3.1, issue
// #154). The set of promoted rule ids is durable broadcast config — it
// must survive a restart/redeploy, exactly like the active-scene pointer
// (show_state, 0004). The boot path (cmd/orion::loadActiveScenes) reseeds
// each promoted rule into the roster after a restart (criterion #11). Only
// the SELECTION is persisted here; a rule's live leaf state always reseeds
// from declared defaults on reload.

// AddStreamRule persists a scene id as a promoted stream rule. Idempotent:
// re-promoting an already-promoted scene leaves the row (and its original
// promoted_at) unchanged rather than erroring.
func (s *Store) AddStreamRule(ctx context.Context, sceneID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO show_stream_rules (scene_id) VALUES ($1)
		   ON CONFLICT (scene_id) DO NOTHING`,
		sceneID,
	)
	if err != nil {
		return fmt.Errorf("add stream rule: %w", err)
	}
	return nil
}

// RemoveStreamRule drops a scene id from the promoted rule set. A no-op
// (zero rows) if the scene was not promoted — demotion is idempotent.
func (s *Store) RemoveStreamRule(ctx context.Context, sceneID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM show_stream_rules WHERE scene_id = $1`,
		sceneID,
	)
	if err != nil {
		return fmt.Errorf("remove stream rule: %w", err)
	}
	return nil
}

// IsStreamRule reports whether a scene id is currently a persisted
// promoted rule.
func (s *Store) IsStreamRule(ctx context.Context, sceneID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM show_stream_rules WHERE scene_id = $1)`,
		sceneID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("is stream rule: %w", err)
	}
	return exists, nil
}

// ListStreamRules returns every promoted rule id, sorted by promotion time
// then id for a deterministic boot-reload order. Used by the boot reseed.
func (s *Store) ListStreamRules(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT scene_id FROM show_stream_rules ORDER BY promoted_at, scene_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list stream rules: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list stream rules scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stream rules rows: %w", err)
	}
	return out, nil
}
