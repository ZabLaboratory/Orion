// Package store is the persistence layer for Orion v2. It wraps a
// pgx connection pool and exposes typed repositories per ADR 004 § 10.
//
// Compiled artifacts (graph + bundle) flow through the store as
// opaque JSON bytes — store doesn't know their schema, the
// compiler/runtime own that. Keeping the store schema-agnostic means
// the runtime can rev the artifact shape without touching SQL.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by every Get-like method when the row is
// absent. Callers compare with errors.Is for routing logic.
var ErrNotFound = errors.New("store: not found")

// Store is the persistence abstraction the rest of Orion depends on
// (ADR 016 §3.2). It is the full method set callers consume; the
// default — and currently only — implementation is *PGStore (Postgres
// via pgx). Extracting the interface lets a second backend (SQLite,
// embedded-local profile, issue #222) plug in at boot without touching
// any caller, while the antenne profile keeps the exact pg path.
//
// Compiled artifacts (graph + bundle) stay opaque JSON bytes through
// every method — no jsonb-specific predicate leaks into the surface —
// so a non-Postgres backend can store them verbatim (ADR 016 §3.2 D2).
//
// The transactional methods name pgx.Tx today: the push/archive paths
// run a multi-statement transaction. That coupling is deliberately left
// for the SQLite work (#222) to generalise; this issue (#218) only
// extracts the interface with behaviour strictly unchanged.
type Store interface {
	Close()
	Pool() *pgxpool.Pool
	Ping(ctx context.Context) error

	// assets
	PutAsset(ctx context.Context, a Asset) (*Asset, error)
	GetAsset(ctx context.Context, id uuid.UUID) (*Asset, error)

	// scenes / definitions / pushed versions
	CreateScene(ctx context.Context, id uuid.UUID, name string) (*Scene, error)
	UpsertScene(ctx context.Context, id uuid.UUID, name string) (*Scene, error)
	GetScene(ctx context.Context, id uuid.UUID) (*Scene, error)
	ListActiveScenesWithPush(ctx context.Context) ([]Scene, error)
	SetSceneStatus(ctx context.Context, id uuid.UUID, status SceneStatus) error
	SetLatestPushedVersion(ctx context.Context, tx pgx.Tx, id uuid.UUID, sceneVersion *string) error
	InsertDefinition(ctx context.Context, def SceneDefinition) error
	InsertDefinitionTx(ctx context.Context, tx pgx.Tx, def SceneDefinition) error
	GetDefinition(ctx context.Context, id uuid.UUID) (*SceneDefinition, error)
	InsertPushedVersion(ctx context.Context, tx pgx.Tx, pv ScenePushedVersion) error
	GetLSMLBundleByHash(ctx context.Context, sceneID uuid.UUID, lsmlHash string) (json.RawMessage, error)
	GetPushedVersion(ctx context.Context, sceneID uuid.UUID, sceneVersion string) (*ScenePushedVersion, error)
	GetLatestPushedVersion(ctx context.Context, sceneID uuid.UUID) (*ScenePushedVersion, error)
	PurgePushedVersions(ctx context.Context, tx pgx.Tx, sceneID uuid.UUID) (int64, error)
	Tx(ctx context.Context, fn func(pgx.Tx) error) error
	MaxDefinitionVersion(ctx context.Context, sceneID uuid.UUID) (int, error)
	NextDefinitionVersionTx(ctx context.Context, tx pgx.Tx, sceneID uuid.UUID) (int, error)

	// show state
	GetActiveSceneID(ctx context.Context) (*uuid.UUID, error)
	SetActiveSceneID(ctx context.Context, id *uuid.UUID) error

	// stream rules
	AddStreamRule(ctx context.Context, sceneID uuid.UUID) error
	RemoveStreamRule(ctx context.Context, sceneID uuid.UUID) error
	IsStreamRule(ctx context.Context, sceneID uuid.UUID) (bool, error)
	ListStreamRules(ctx context.Context) ([]uuid.UUID, error)

	// validations
	UpsertValidation(ctx context.Context, v SceneValidation) error
	GetValidation(ctx context.Context, sceneID uuid.UUID, sceneVersion, harnessVersion string) (*SceneValidation, error)
	IsVersionValidated(ctx context.Context, sceneID uuid.UUID, sceneVersion, harnessVersion string) (bool, error)
}

// PGStore is the Postgres-backed Store: a pgx connection pool exposing
// typed repositories per ADR 004 § 10. It is the antenne-profile
// default and, until #222, the only implementation.
type PGStore struct {
	pool *pgxpool.Pool
}

// compile-time assertion: PGStore satisfies the Store interface.
var _ Store = (*PGStore)(nil)

// Open opens a pgx pool and pings to confirm the database is reachable.
func Open(ctx context.Context, dsn string) (*PGStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &PGStore{pool: pool}, nil
}

// Close releases the connection pool.
func (s *PGStore) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Pool exposes the underlying pgxpool. Used sparingly — most callers
// reach for typed methods on the store, but adapters/pg-listen needs
// a raw connection acquire.
func (s *PGStore) Pool() *pgxpool.Pool { return s.pool }

// Ping is wired into /api/v1/ready.
func (s *PGStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// noRow normalises pgx's ErrNoRows into our ErrNotFound.
func noRow(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
