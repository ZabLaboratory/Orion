package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO; keeps the static binary)
)

// SQLiteStore is the embedded-local Store backend (ADR 016 §3.2 / RC-3): a
// single-file SQLite database that mirrors PGStore behaviour for every
// operation the boot/exec paths use. It is selected at boot only in the
// embedded-local profile; the antenne default stays *PGStore unchanged.
//
// Parity rule: SQLiteStore stores every compiled artefact as OPAQUE TEXT —
// no jsonb predicate, no ->>, no json1 walk — exactly as PGStore keeps them
// opaque (the store-level half of D2). uuids serialise to their canonical
// lowercase form and timestamps to RFC3339, so the Go-typed values callers
// read back are identical to the pg path. The scalar-coercion / NULL-ordering
// parity work the Conduit contract flags (A.6) is the _query sidecar's job
// (#225), not this store's: the store never sorts or coerces a domain scalar,
// it round-trips opaque blobs and a handful of typed columns.
type SQLiteStore struct {
	db *sql.DB
}

// compile-time assertion: SQLiteStore satisfies the Store interface.
var _ Store = (*SQLiteStore)(nil)

//go:embed sqlite_migrations/*.sql
var sqliteMigrations embed.FS

// OpenSQLite opens (creating if absent) the local SQLite database at path
// and applies the embedded schema. foreign_keys is enabled per-connection
// (SQLite defaults it off) so the ON DELETE CASCADE / SET NULL clauses that
// mirror the pg schema actually fire. busy_timeout guards the single-writer
// model against a transient lock under the boot reseed.
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: sqlite ping: %w", err)
	}
	s := &SQLiteStore{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate applies every embedded SQLite migration once, tracked in a
// schema_migrations table (goose-equivalent, minimal — the schema is a
// handful of tables, ADR 016 §3.2). Each file is applied in a transaction
// and recorded by name so a re-open is a no-op.
func (s *SQLiteStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("store: sqlite migrations table: %w", err)
	}
	entries, err := sqliteMigrations.ReadDir("sqlite_migrations")
	if err != nil {
		return fmt.Errorf("store: read sqlite migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names) // lexical order == migration order (0001, 0002, …)
	for _, name := range names {
		var seen string
		err := s.db.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE name = ?`, name).Scan(&seen)
		if err == nil {
			continue // already applied
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: check migration %s: %w", name, err)
		}
		body, rerr := sqliteMigrations.ReadFile("sqlite_migrations/" + name)
		if rerr != nil {
			return fmt.Errorf("store: read migration %s: %w", name, rerr)
		}
		tx, terr := s.db.BeginTx(ctx, nil)
		if terr != nil {
			return fmt.Errorf("store: begin migration %s: %w", name, terr)
		}
		if _, eerr := tx.ExecContext(ctx, string(body)); eerr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: apply migration %s: %w", name, eerr)
		}
		if _, eerr := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, nowRFC3339()); eerr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: record migration %s: %w", name, eerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return fmt.Errorf("store: commit migration %s: %w", name, cerr)
		}
	}
	return nil
}

func (s *SQLiteStore) Close() {
	if s.db != nil {
		_ = s.db.Close()
	}
}

// Pool returns nil: the pgxpool exists only for the pg-listen adapter, which
// is wired in the antenne profile alone. embedded-local never builds it, so
// no caller dereferences this (cmd/orion guards the adapter on profile).
func (s *SQLiteStore) Pool() *pgxpool.Pool { return nil }

func (s *SQLiteStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// nowRFC3339 formats the current UTC instant the way the schema defaults do,
// so an app-written timestamp and a DEFAULT-written one parse identically.
func nowRFC3339() string { return time.Now().UTC().Format(timeLayout) }

// timeLayout is the RFC3339-with-millis form the SQLite schema stores
// (strftime '%Y-%m-%dT%H:%M:%fZ'); parseTime accepts a couple of variants.
const timeLayout = "2006-01-02T15:04:05.000Z"

// parseTime decodes a stored timestamp string into time.Time. SQLite stores
// text; the column DEFAULT and nowRFC3339 both emit RFC3339-millis-Z, but we
// accept the no-millis and space-separated forms defensively.
func parseTime(s string) time.Time {
	for _, layout := range []string{timeLayout, time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// noRowSQL normalises database/sql's ErrNoRows into our ErrNotFound, mirroring
// noRow for the pgx path so callers' errors.Is(err, ErrNotFound) is backend-
// agnostic.
func noRowSQL(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ---- sqlTx: store-neutral Tx over a *sql.Tx ------------------------------

// sqlTx adapts a *sql.Tx to the store-neutral Tx interface, mirroring pgxTx.
// The SQLite Store methods write `?`-placeholder SQL (the SQLite dialect),
// so the SQL the tx executes is dialect text the driver speaks directly.
type sqlTx struct {
	ctx context.Context
	tx  *sql.Tx
}

func (t sqlTx) Exec(_ context.Context, query string, args ...any) (int64, error) {
	res, err := t.tx.ExecContext(t.ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (t sqlTx) QueryRow(_ context.Context, query string, args ...any) Row {
	return t.tx.QueryRowContext(t.ctx, query, args...)
}

// Tx runs fn inside a SQLite transaction, adapting *sql.Tx to store.Tx.
func (s *SQLiteStore) Tx(ctx context.Context, fn func(Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(sqlTx{ctx: ctx, tx: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---- assets --------------------------------------------------------------

func (s *SQLiteStore) PutAsset(ctx context.Context, a Asset) (*Asset, error) {
	// SQLite UPSERT mirrors pg's ON CONFLICT (sha256_hex) DO UPDATE.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO assets (id, sha256_hex, mime, size_bytes, filesystem_path)
		   VALUES (?, ?, ?, ?, ?)
		   ON CONFLICT (sha256_hex) DO UPDATE SET mime = excluded.mime`,
		a.ID.String(), a.SHA256Hex, a.Mime, a.SizeBytes, a.FilesystemPath,
	)
	if err != nil {
		return nil, err
	}
	return s.assetByHash(ctx, a.SHA256Hex)
}

func (s *SQLiteStore) assetByHash(ctx context.Context, hash string) (*Asset, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, sha256_hex, mime, size_bytes, filesystem_path, created_at
		   FROM assets WHERE sha256_hex = ?`, hash)
	return scanAssetSQL(row)
}

func (s *SQLiteStore) GetAsset(ctx context.Context, id uuid.UUID) (*Asset, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, sha256_hex, mime, size_bytes, filesystem_path, created_at
		   FROM assets WHERE id = ?`, id.String())
	return scanAssetSQL(row)
}

func scanAssetSQL(row Row) (*Asset, error) {
	var (
		a       Asset
		idStr   string
		created string
	)
	if err := row.Scan(&idStr, &a.SHA256Hex, &a.Mime, &a.SizeBytes, &a.FilesystemPath, &created); err != nil {
		return nil, noRowSQL(err)
	}
	a.ID = uuid.MustParse(idStr)
	a.CreatedAt = parseTime(created)
	return &a, nil
}

// ---- scenes / definitions / pushed versions ------------------------------

func (s *SQLiteStore) CreateScene(ctx context.Context, id uuid.UUID, name string) (*Scene, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scenes (id, name, status) VALUES (?, ?, 'active')`,
		id.String(), name); err != nil {
		return nil, err
	}
	return s.GetScene(ctx, id)
}

func (s *SQLiteStore) UpsertScene(ctx context.Context, id uuid.UUID, name string) (*Scene, error) {
	// Idempotent first-push create; a no-op on an existing row (name/status/
	// pointer preserved), mirroring pg's ON CONFLICT DO UPDATE SET id=id.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scenes (id, name, status) VALUES (?, ?, 'active')
		   ON CONFLICT (id) DO NOTHING`,
		id.String(), name); err != nil {
		return nil, err
	}
	return s.GetScene(ctx, id)
}

func (s *SQLiteStore) GetScene(ctx context.Context, id uuid.UUID) (*Scene, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, status, latest_pushed_version, created_at, updated_at
		   FROM scenes WHERE id = ?`, id.String())
	return scanSceneSQL(row)
}

func (s *SQLiteStore) ListActiveScenesWithPush(ctx context.Context) ([]Scene, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, status, latest_pushed_version, created_at, updated_at
		   FROM scenes
		  WHERE status = 'active' AND latest_pushed_version IS NOT NULL
		  ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list active scenes: %w", err)
	}
	defer rows.Close()
	var out []Scene
	for rows.Next() {
		sc, err := scanSceneSQL(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sc)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SetSceneStatus(ctx context.Context, id uuid.UUID, status SceneStatus) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE scenes SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), nowRFC3339(), id.String())
	if err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) SetLatestPushedVersion(_ context.Context, tx Tx, id uuid.UUID, sceneVersion *string) error {
	n, err := tx.Exec(context.Background(),
		`UPDATE scenes SET latest_pushed_version = ?, updated_at = ? WHERE id = ?`,
		sceneVersion, nowRFC3339(), id.String())
	if err != nil {
		return fmt.Errorf("set latest pushed: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) InsertDefinition(ctx context.Context, def SceneDefinition) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO scene_definitions (id, scene_id, definition_version,
		    canvas_version, blue_blueprint_id, components_jsonb, created_at)
		   VALUES (?, ?, ?, ?, ?, ?, ?)`,
		def.ID.String(), def.SceneID.String(), def.DefinitionVersion, def.CanvasVersion,
		def.BlueBlueprintID, rawOrEmpty(def.ComponentsJSON, "[]"), timeOrNow(def.CreatedAt))
	return err
}

func (s *SQLiteStore) InsertDefinitionTx(_ context.Context, tx Tx, def SceneDefinition) error {
	_, err := tx.Exec(context.Background(),
		`INSERT INTO scene_definitions (id, scene_id, definition_version,
		    canvas_version, blue_blueprint_id, components_jsonb, created_at)
		   VALUES (?, ?, ?, ?, ?, ?, ?)`,
		def.ID.String(), def.SceneID.String(), def.DefinitionVersion, def.CanvasVersion,
		def.BlueBlueprintID, rawOrEmpty(def.ComponentsJSON, "[]"), timeOrNow(def.CreatedAt))
	return err
}

func (s *SQLiteStore) GetDefinition(ctx context.Context, id uuid.UUID) (*SceneDefinition, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, scene_id, definition_version, canvas_version,
		        blue_blueprint_id, components_jsonb, created_at
		   FROM scene_definitions WHERE id = ?`, id.String())
	return scanDefinitionSQL(row)
}

func (s *SQLiteStore) InsertPushedVersion(_ context.Context, tx Tx, pv ScenePushedVersion) error {
	_, err := tx.Exec(context.Background(),
		`INSERT INTO scene_pushed_versions (scene_id, scene_version, definition_id,
		    graph_jsonb, bundle_jsonb, lsml_bundle_jsonb, lsml_bundle_hash, created_at)
		   VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		   ON CONFLICT (scene_id, scene_version) DO NOTHING`,
		pv.SceneID.String(), pv.SceneVersion, pv.DefinitionID.String(),
		rawOrEmpty(pv.GraphJSON, ""), rawOrEmpty(pv.BundleJSON, ""),
		nullableRaw(pv.LSMLBundleJSON), pv.LSMLBundleHash, timeOrNow(pv.CreatedAt))
	return err
}

func (s *SQLiteStore) GetLSMLBundleByHash(ctx context.Context, sceneID uuid.UUID, lsmlHash string) (json.RawMessage, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT lsml_bundle_jsonb FROM scene_pushed_versions
		  WHERE scene_id = ? AND lsml_bundle_hash = ? LIMIT 1`,
		sceneID.String(), lsmlHash).Scan(&raw)
	if err != nil {
		return nil, noRowSQL(err)
	}
	return json.RawMessage(raw), nil
}

func (s *SQLiteStore) GetPushedVersion(ctx context.Context, sceneID uuid.UUID, sceneVersion string) (*ScenePushedVersion, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT scene_id, scene_version, definition_id, graph_jsonb, bundle_jsonb, created_at
		   FROM scene_pushed_versions WHERE scene_id = ? AND scene_version = ?`,
		sceneID.String(), sceneVersion)
	return scanPushedVersionSQL(row)
}

func (s *SQLiteStore) GetLatestPushedVersion(ctx context.Context, sceneID uuid.UUID) (*ScenePushedVersion, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT pv.scene_id, pv.scene_version, pv.definition_id, pv.graph_jsonb, pv.bundle_jsonb, pv.created_at
		   FROM scenes s
		   JOIN scene_pushed_versions pv
		     ON pv.scene_id = s.id AND pv.scene_version = s.latest_pushed_version
		  WHERE s.id = ?`, sceneID.String())
	return scanPushedVersionSQL(row)
}

func (s *SQLiteStore) PurgePushedVersions(_ context.Context, tx Tx, sceneID uuid.UUID) (int64, error) {
	return tx.Exec(context.Background(),
		`DELETE FROM scene_pushed_versions WHERE scene_id = ?`, sceneID.String())
}

func (s *SQLiteStore) MaxDefinitionVersion(ctx context.Context, sceneID uuid.UUID) (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(definition_version) FROM scene_definitions WHERE scene_id = ?`,
		sceneID.String()).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// NextDefinitionVersionTx mirrors the pg race-safe sequencer. SQLite is a
// single writer (the WAL + busy_timeout serialise writers), and the whole
// push runs inside one tx, so MAX+1 inside the tx is already collision-free;
// there is no FOR UPDATE in SQLite (and none is needed — concurrent writers
// block at BEGIN). The scenes-row existence check is kept for parity.
func (s *SQLiteStore) NextDefinitionVersionTx(_ context.Context, tx Tx, sceneID uuid.UUID) (int, error) {
	var exists string
	if err := tx.QueryRow(context.Background(),
		`SELECT id FROM scenes WHERE id = ?`, sceneID.String()).Scan(&exists); err != nil {
		return 0, noRowSQL(err)
	}
	var v sql.NullInt64
	if err := tx.QueryRow(context.Background(),
		`SELECT MAX(definition_version) FROM scene_definitions WHERE scene_id = ?`,
		sceneID.String()).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 1, nil
	}
	return int(v.Int64) + 1, nil
}

// ---- show state ----------------------------------------------------------

func (s *SQLiteStore) GetActiveSceneID(ctx context.Context) (*uuid.UUID, error) {
	var idStr sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT active_scene_id FROM show_state WHERE id = 1`).Scan(&idStr); err != nil {
		return nil, fmt.Errorf("get active scene id: %w", err)
	}
	if !idStr.Valid {
		return nil, nil
	}
	id, err := uuid.Parse(idStr.String)
	if err != nil {
		return nil, fmt.Errorf("get active scene id: parse: %w", err)
	}
	return &id, nil
}

func (s *SQLiteStore) SetActiveSceneID(ctx context.Context, id *uuid.UUID) error {
	var arg any
	if id != nil {
		arg = id.String()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE show_state SET active_scene_id = ?, updated_at = ? WHERE id = 1`,
		arg, nowRFC3339())
	if err != nil {
		return fmt.Errorf("set active scene id: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO show_state (id, active_scene_id) VALUES (1, ?)
			   ON CONFLICT (id) DO UPDATE SET active_scene_id = excluded.active_scene_id, updated_at = ?`,
			arg, nowRFC3339()); err != nil {
			return fmt.Errorf("set active scene id (insert): %w", err)
		}
	}
	return nil
}

// ---- stream rules --------------------------------------------------------

func (s *SQLiteStore) AddStreamRule(ctx context.Context, sceneID uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO show_stream_rules (scene_id) VALUES (?)
		   ON CONFLICT (scene_id) DO NOTHING`, sceneID.String()); err != nil {
		return fmt.Errorf("add stream rule: %w", err)
	}
	return nil
}

func (s *SQLiteStore) RemoveStreamRule(ctx context.Context, sceneID uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM show_stream_rules WHERE scene_id = ?`, sceneID.String()); err != nil {
		return fmt.Errorf("remove stream rule: %w", err)
	}
	return nil
}

func (s *SQLiteStore) IsStreamRule(ctx context.Context, sceneID uuid.UUID) (bool, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM show_stream_rules WHERE scene_id = ?)`,
		sceneID.String()).Scan(&exists); err != nil {
		return false, fmt.Errorf("is stream rule: %w", err)
	}
	return exists == 1, nil
}

func (s *SQLiteStore) ListStreamRules(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT scene_id FROM show_stream_rules ORDER BY promoted_at, scene_id`)
	if err != nil {
		return nil, fmt.Errorf("list stream rules: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var idStr string
		if err := rows.Scan(&idStr); err != nil {
			return nil, fmt.Errorf("list stream rules scan: %w", err)
		}
		id, perr := uuid.Parse(idStr)
		if perr != nil {
			return nil, fmt.Errorf("list stream rules parse: %w", perr)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stream rules rows: %w", err)
	}
	return out, nil
}

// ---- validations ---------------------------------------------------------

func (s *SQLiteStore) UpsertValidation(ctx context.Context, v SceneValidation) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO scene_validations
		   (scene_id, scene_version, harness_version, status, report, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (scene_id, scene_version, harness_version)
		 DO UPDATE SET status = excluded.status,
		               report = excluded.report,
		               created_at = excluded.created_at`,
		v.SceneID.String(), v.SceneVersion, v.HarnessVersion, v.Status,
		rawOrEmpty(v.Report, "{}"), timeOrNow(v.CreatedAt))
	if err != nil {
		return fmt.Errorf("upsert validation: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetValidation(ctx context.Context, sceneID uuid.UUID, sceneVersion, harnessVersion string) (*SceneValidation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT scene_id, scene_version, harness_version, status, report, created_at
		   FROM scene_validations
		  WHERE scene_id = ? AND scene_version = ? AND harness_version = ?`,
		sceneID.String(), sceneVersion, harnessVersion)
	var (
		v       SceneValidation
		idStr   string
		report  []byte
		created string
	)
	if err := row.Scan(&idStr, &v.SceneVersion, &v.HarnessVersion, &v.Status, &report, &created); err != nil {
		return nil, noRowSQL(err)
	}
	v.SceneID = uuid.MustParse(idStr)
	v.Report = json.RawMessage(report)
	v.CreatedAt = parseTime(created)
	return &v, nil
}

func (s *SQLiteStore) IsVersionValidated(ctx context.Context, sceneID uuid.UUID, sceneVersion, harnessVersion string) (bool, error) {
	v, err := s.GetValidation(ctx, sceneID, sceneVersion, harnessVersion)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v.Status == ValidationValidated, nil
}

// ---- row scanners + helpers ---------------------------------------------

func scanSceneSQL(row Row) (*Scene, error) {
	var (
		sc      Scene
		idStr   string
		latest  sql.NullString
		status  string
		created string
		updated string
	)
	if err := row.Scan(&idStr, &sc.Name, &status, &latest, &created, &updated); err != nil {
		return nil, noRowSQL(err)
	}
	sc.ID = uuid.MustParse(idStr)
	sc.Status = SceneStatus(status)
	if latest.Valid {
		v := latest.String
		sc.LatestPushedVersion = &v
	}
	sc.CreatedAt = parseTime(created)
	sc.UpdatedAt = parseTime(updated)
	return &sc, nil
}

func scanDefinitionSQL(row Row) (*SceneDefinition, error) {
	var (
		d          SceneDefinition
		idStr      string
		sceneStr   string
		components []byte
		created    string
	)
	if err := row.Scan(&idStr, &sceneStr, &d.DefinitionVersion, &d.CanvasVersion,
		&d.BlueBlueprintID, &components, &created); err != nil {
		return nil, noRowSQL(err)
	}
	d.ID = uuid.MustParse(idStr)
	d.SceneID = uuid.MustParse(sceneStr)
	d.ComponentsJSON = json.RawMessage(components)
	d.CreatedAt = parseTime(created)
	return &d, nil
}

func scanPushedVersionSQL(row Row) (*ScenePushedVersion, error) {
	var (
		p        ScenePushedVersion
		sceneStr string
		defStr   string
		graph    []byte
		bundle   []byte
		created  string
	)
	if err := row.Scan(&sceneStr, &p.SceneVersion, &defStr, &graph, &bundle, &created); err != nil {
		return nil, noRowSQL(err)
	}
	p.SceneID = uuid.MustParse(sceneStr)
	p.DefinitionID = uuid.MustParse(defStr)
	p.GraphJSON = json.RawMessage(graph)
	p.BundleJSON = json.RawMessage(bundle)
	p.CreatedAt = parseTime(created)
	return &p, nil
}

// rawOrEmpty renders an opaque JSON blob as the TEXT to store, substituting a
// dialect default when nil (mirrors the column DEFAULT so a nil components
// list reads back as "[]" just like pg).
func rawOrEmpty(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	return string(raw)
}

// nullableRaw renders an optional JSON blob, NULL when absent (mirrors pg
// binding a nil json.RawMessage as SQL NULL).
func nullableRaw(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// timeOrNow stores a caller-supplied timestamp, defaulting to now when zero.
func timeOrNow(t time.Time) string {
	if t.IsZero() {
		return nowRFC3339()
	}
	return t.UTC().Format(timeLayout)
}
