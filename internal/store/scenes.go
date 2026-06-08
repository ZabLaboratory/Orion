package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
//
// LSMLBundleJSON / LSMLBundleHash are the additive Lumencast-convergence
// columns (ADR 007 §C.2). They are nil/empty for versions pushed in
// `bespoke` mode (the default) and only populated when ORION_LSDP_MODE
// is dual|lsdp. The bespoke GraphJSON/BundleJSON path is unaffected.
type ScenePushedVersion struct {
	SceneID        uuid.UUID
	SceneVersion   string
	DefinitionID   uuid.UUID
	GraphJSON      json.RawMessage
	BundleJSON     json.RawMessage
	LSMLBundleJSON json.RawMessage // nil unless LSML persisted (dual|lsdp)
	LSMLBundleHash *string         // the LSML content address ("sha256:<hex>")
	CreatedAt      time.Time
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

// UpsertScene ensures a scenes row exists for id, idempotently. On a
// first push it creates the row (status=active, latest_pushed_version=
// NULL, name=placeholder — Canvas owns the canonical name, ADR 002
// §3.2). On a subsequent push it is a no-op and returns the existing
// row unchanged (existing name/status/pointer preserved).
//
// The ON CONFLICT clause uses DO UPDATE SET id = scenes.id (a no-op
// write to the PK) rather than DO NOTHING deliberately (ADR 002 §3.1):
// DO NOTHING does not return a row via RETURNING, so a concurrent
// first-push that loses the insert race would get zero rows and have
// to re-SELECT. The no-op DO UPDATE makes RETURNING always yield the
// surviving row in one statement — idempotent, race-safe (R2), one
// round-trip. It touches only id (to itself): name, status, and
// latest_pushed_version are never overwritten on an existing scene, so
// an archived scene stays archived and the caller's archived guard runs
// on the real row.
func (s *Store) UpsertScene(ctx context.Context, id uuid.UUID, name string) (*Scene, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO scenes (id, name, status) VALUES ($1, $2, 'active')
		   ON CONFLICT (id) DO UPDATE SET id = scenes.id
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

// InsertDefinition records a save on the pool directly. definition_version
// is supplied by the caller. This is the single-writer convenience path
// (e.g. the e2e seed in tests/e2e/push_test.go); the push handler uses
// InsertDefinitionTx so the MAX(definition_version)+1 read and this insert
// share one transaction and one scenes-row lock (R2 — see NextDefinitionVersionTx).
func (s *Store) InsertDefinition(ctx context.Context, def SceneDefinition) error {
	return insertDefinition(ctx, s.pool, def)
}

// InsertDefinitionTx is the transactional twin of InsertDefinition. The push
// handler calls it inside the same Store.Tx that locked the scenes row and
// computed def.DefinitionVersion via NextDefinitionVersionTx, so two concurrent
// first-pushes can never both read MAX=0 and collide on
// UNIQUE(scene_id, definition_version).
func (s *Store) InsertDefinitionTx(ctx context.Context, tx pgx.Tx, def SceneDefinition) error {
	return insertDefinition(ctx, tx, def)
}

// querier is the common surface of *pgxpool.Pool and pgx.Tx the definition
// inserts share, so the SQL lives in one place.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertDefinition(ctx context.Context, q querier, def SceneDefinition) error {
	_, err := q.Exec(ctx,
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
	// lsml_bundle_jsonb / lsml_bundle_hash are nil/NULL in bespoke mode;
	// pgx binds a nil json.RawMessage / *string as SQL NULL, so the
	// additive columns stay empty unless the caller populated them.
	_, err := tx.Exec(ctx,
		`INSERT INTO scene_pushed_versions (scene_id, scene_version, definition_id,
		    graph_jsonb, bundle_jsonb, lsml_bundle_jsonb, lsml_bundle_hash, created_at)
		   VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		   ON CONFLICT (scene_id, scene_version) DO NOTHING`,
		pv.SceneID, pv.SceneVersion, pv.DefinitionID, pv.GraphJSON, pv.BundleJSON,
		pv.LSMLBundleJSON, pv.LSMLBundleHash, pv.CreatedAt,
	)
	return err
}

// GetLSMLBundleByHash fetches the persisted LSML bundle bytes for a
// scene, content-addressed by the LSML hash ("sha256:<hex>"). Returns
// ErrNotFound when no pushed version of the scene carries that hash —
// the by-hash GET path translates that to 404 (ADR 007 §C.2). Only ever
// returns rows whose lsml_bundle_hash is non-NULL, so bespoke-mode
// versions are invisible to this lookup.
func (s *Store) GetLSMLBundleByHash(ctx context.Context, sceneID uuid.UUID, lsmlHash string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.pool.QueryRow(ctx,
		`SELECT lsml_bundle_jsonb
		   FROM scene_pushed_versions
		  WHERE scene_id = $1 AND lsml_bundle_hash = $2
		  LIMIT 1`,
		sceneID, lsmlHash,
	).Scan(&raw)
	if err != nil {
		return nil, noRow(err)
	}
	return raw, nil
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
// for the scene, or 0 if none yet. Pool-scoped read — no lock; safe only
// where the caller is the single writer for the scene. The push handler
// uses NextDefinitionVersionTx instead.
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

// NextDefinitionVersionTx locks the scenes row (SELECT ... FOR UPDATE) and
// returns MAX(definition_version)+1 for the scene, all inside the caller's
// transaction. The row lock serialises concurrent push writers on the same
// scene_id: the second writer blocks on the FOR UPDATE until the first
// commits its InsertDefinitionTx, then reads the now-incremented MAX. This
// closes the read-then-insert race that let N concurrent first-pushes each
// compute version 1 and collide on UNIQUE(scene_id, definition_version)
// → Postgres 23505 → 500 (Probe #56 / PR #61, R2).
//
// The lock is taken on the scenes row (not scene_definitions) because the
// scene_definitions rows being counted may not exist yet on a first push —
// there is nothing to lock there. The scenes row is guaranteed to exist:
// the push handler's UpsertScene created it earlier in the same request.
// ErrNotFound if the scene row is absent (defensive — the caller upserts
// first, so this should not happen on the push path).
func (s *Store) NextDefinitionVersionTx(ctx context.Context, tx pgx.Tx, sceneID uuid.UUID) (int, error) {
	// Take the row lock first. The result is discarded; FOR UPDATE is the
	// point. A separate aggregate query then reads the current MAX under
	// the lock the loser is now waiting on.
	var locked uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM scenes WHERE id = $1 FOR UPDATE`, sceneID,
	).Scan(&locked)
	if err != nil {
		return 0, noRow(err)
	}

	var v *int
	err = tx.QueryRow(ctx,
		`SELECT MAX(definition_version) FROM scene_definitions WHERE scene_id = $1`,
		sceneID,
	).Scan(&v)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 1, nil
	}
	return *v + 1, nil
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
