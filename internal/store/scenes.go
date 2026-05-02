package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SceneStatus matches the CHECK in migrations/0001_init.sql.
type SceneStatus string

const (
	SceneActive   SceneStatus = "active"
	SceneArchived SceneStatus = "archived"
)

// Scene is the row shape from the `scenes` table.
type Scene struct {
	ID                  uuid.UUID
	Name                string
	Status              SceneStatus
	LatestPushedVersion *string // nil until the scene has been pushed at least once
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// SceneDefinition is the row shape from `scene_definitions`. The
// components blob is opaque JSON owned by the compiler.
type SceneDefinition struct {
	ID                uuid.UUID
	SceneID           uuid.UUID
	DefinitionVersion int
	CanvasVersion     string
	BlueBlueprintID   string
	ComponentsJSON    json.RawMessage
	CreatedAt         time.Time
}

// ScenePushedVersion holds an immutable compiled artifact pair.
type ScenePushedVersion struct {
	SceneID      uuid.UUID
	SceneVersion string
	DefinitionID uuid.UUID
	GraphJSON    json.RawMessage
	BundleJSON   json.RawMessage
	CreatedAt    time.Time
}

// ErrSceneInUse is returned when an operator tries to archive the
// currently active scene. ADR 004 § 10.1, criterion 14.
var ErrSceneInUse = errors.New("store: scene in use")

// CreateScene inserts a fresh scene with status=active, no pushed
// version. The first push is what makes the scene runnable.
func (s *Store) CreateScene(ctx context.Context, id uuid.UUID, name string) (*Scene, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO scenes (id, name, status) VALUES ($1, $2, 'active')
		   RETURNING id, name, status, latest_pushed_version, created_at, updated_at`,
		id, name,
	)
	return scanScene(row)
}

// GetScene fetches one scene by id.
func (s *Store) GetScene(ctx context.Context, id uuid.UUID) (*Scene, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, name, status, latest_pushed_version, created_at, updated_at
		   FROM scenes WHERE id = $1`, id,
	)
	return scanScene(row)
}

// ListActiveScenesWithPush returns every scene Orion should bring
// alive at startup (status=active and latest_pushed_version not null).
// ADR 004 § 4.4.
func (s *Store) ListActiveScenesWithPush(ctx context.Context) ([]Scene, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, status, latest_pushed_version, created_at, updated_at
		   FROM scenes
		   WHERE status = 'active' AND latest_pushed_version IS NOT NULL
		   ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list active scenes: %w", err)
	}
	defer rows.Close()

	var out []Scene
	for rows.Next() {
		sc, err := scanScene(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sc)
	}
	return out, rows.Err()
}

// SetSceneStatus transitions a scene between active and archived.
// Archiving the currently active scene is rejected at the API layer
// (ADR 004 § 14); this method just enforces the schema constraint.
// On archive, the caller is expected to call PurgePushedVersions in
// the same transaction so § 10.1's invariant holds.
func (s *Store) SetSceneStatus(ctx context.Context, id uuid.UUID, status SceneStatus) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE scenes SET status = $2, updated_at = now() WHERE id = $1`,
		id, status,
	)
	if err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetLatestPushedVersion advances the pointer the runtime reads.
// Callers wrap this in the same transaction that wrote the pushed
// version artefact so an external observer never sees a pointer
// pointing at a non-existent (scene_id, scene_version).
func (s *Store) SetLatestPushedVersion(ctx context.Context, tx pgx.Tx, id uuid.UUID, sceneVersion *string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE scenes SET latest_pushed_version = $2, updated_at = now() WHERE id = $1`,
		id, sceneVersion,
	)
	if err != nil {
		return fmt.Errorf("set latest pushed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// InsertDefinition records a save. definition_version is monotonically
// computed by the caller (max+1 strategy is fine — saves are
// single-writer per scene from Canvas's perspective).
func (s *Store) InsertDefinition(ctx context.Context, def SceneDefinition) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO scene_definitions (id, scene_id, definition_version,
		    canvas_version, blue_blueprint_id, components_jsonb, created_at)
		   VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		def.ID, def.SceneID, def.DefinitionVersion, def.CanvasVersion,
		def.BlueBlueprintID, def.ComponentsJSON, def.CreatedAt,
	)
	return err
}

// GetDefinition fetches a specific definition. ErrNotFound on miss.
func (s *Store) GetDefinition(ctx context.Context, id uuid.UUID) (*SceneDefinition, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, scene_id, definition_version, canvas_version,
		        blue_blueprint_id, components_jsonb, created_at
		   FROM scene_definitions WHERE id = $1`, id,
	)
	return scanDefinition(row)
}

// InsertPushedVersion + SetLatestPushedVersion are typically called
// together inside a single tx — see Store.Tx helper.
func (s *Store) InsertPushedVersion(ctx context.Context, tx pgx.Tx, pv ScenePushedVersion) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO scene_pushed_versions (scene_id, scene_version, definition_id,
		    graph_jsonb, bundle_jsonb, created_at)
		   VALUES ($1, $2, $3, $4, $5, $6)
		   ON CONFLICT (scene_id, scene_version) DO NOTHING`,
		pv.SceneID, pv.SceneVersion, pv.DefinitionID, pv.GraphJSON, pv.BundleJSON, pv.CreatedAt,
	)
	return err
}

// GetPushedVersion fetches the named version (rollback target reads
// or test mode pin). ErrNotFound if the row doesn't exist (caller
// translates to SCENE_NOT_PUSHED at the API level).
func (s *Store) GetPushedVersion(ctx context.Context, sceneID uuid.UUID, sceneVersion string) (*ScenePushedVersion, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT scene_id, scene_version, definition_id, graph_jsonb, bundle_jsonb, created_at
		   FROM scene_pushed_versions WHERE scene_id = $1 AND scene_version = $2`,
		sceneID, sceneVersion,
	)
	return scanPushedVersion(row)
}

// GetLatestPushedVersion resolves the scene's latest_pushed_version
// pointer and fetches the artefact in one go.
func (s *Store) GetLatestPushedVersion(ctx context.Context, sceneID uuid.UUID) (*ScenePushedVersion, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT pv.scene_id, pv.scene_version, pv.definition_id, pv.graph_jsonb, pv.bundle_jsonb, pv.created_at
		   FROM scenes s
		   JOIN scene_pushed_versions pv
		     ON pv.scene_id = s.id AND pv.scene_version = s.latest_pushed_version
		   WHERE s.id = $1`, sceneID,
	)
	return scanPushedVersion(row)
}

// PurgePushedVersions deletes every compiled artefact for a scene.
// Used on archive (§ 10.1).
func (s *Store) PurgePushedVersions(ctx context.Context, tx pgx.Tx, sceneID uuid.UUID) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM scene_pushed_versions WHERE scene_id = $1`, sceneID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Tx runs fn inside a transaction. Wraps pgx's BeginFunc with our
// own naming so callers don't need to import pgx directly.
func (s *Store) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// MaxDefinitionVersion returns the highest definition_version stored
// for the scene, or 0 if none yet.
func (s *Store) MaxDefinitionVersion(ctx context.Context, sceneID uuid.UUID) (int, error) {
	var v *int
	err := s.pool.QueryRow(ctx,
		`SELECT MAX(definition_version) FROM scene_definitions WHERE scene_id = $1`,
		sceneID,
	).Scan(&v)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}

func scanScene(row pgx.Row) (*Scene, error) {
	var s Scene
	err := row.Scan(&s.ID, &s.Name, &s.Status, &s.LatestPushedVersion, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, noRow(err)
	}
	return &s, nil
}

func scanDefinition(row pgx.Row) (*SceneDefinition, error) {
	var d SceneDefinition
	err := row.Scan(&d.ID, &d.SceneID, &d.DefinitionVersion, &d.CanvasVersion,
		&d.BlueBlueprintID, &d.ComponentsJSON, &d.CreatedAt)
	if err != nil {
		return nil, noRow(err)
	}
	return &d, nil
}

func scanPushedVersion(row pgx.Row) (*ScenePushedVersion, error) {
	var p ScenePushedVersion
	err := row.Scan(&p.SceneID, &p.SceneVersion, &p.DefinitionID,
		&p.GraphJSON, &p.BundleJSON, &p.CreatedAt)
	if err != nil {
		return nil, noRow(err)
	}
	return &p, nil
}
