package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// GetActiveSceneID reads the persisted live-antenna pointer (the scene
// SELECTION that survives a restart, migrations/0004). Returns nil when no
// scene is on air (the singleton row's active_scene_id is NULL, or — after
// ON DELETE SET NULL — the on-air scene was archived/deleted). The boot path
// (cmd/orion/main.go::loadActiveScenes) uses this to re-activate the same
// scene after a redeploy so the antenna survives.
func (s *PGStore) GetActiveSceneID(ctx context.Context) (*uuid.UUID, error) {
	var id *uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT active_scene_id FROM show_state WHERE id = TRUE`,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("get active scene id: %w", err)
	}
	return id, nil
}

// SetActiveSceneID persists the live-antenna pointer. Pass nil to clear it
// (no scene on air). Updates the singleton show_state row seeded by the
// migration, so this is always an UPDATE that touches exactly one row.
func (s *PGStore) SetActiveSceneID(ctx context.Context, id *uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE show_state SET active_scene_id = $1, updated_at = now() WHERE id = TRUE`,
		id,
	)
	if err != nil {
		return fmt.Errorf("set active scene id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Defensive: the migration seeds the singleton row, so this should
		// never happen. Insert it rather than silently lose the pointer.
		_, err = s.pool.Exec(ctx,
			`INSERT INTO show_state (id, active_scene_id) VALUES (TRUE, $1)
			   ON CONFLICT (id) DO UPDATE SET active_scene_id = $1, updated_at = now()`,
			id,
		)
		if err != nil {
			return fmt.Errorf("set active scene id (insert): %w", err)
		}
	}
	return nil
}
