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
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by every Get-like method when the row is
// absent. Callers compare with errors.Is for routing logic.
var ErrNotFound = errors.New("store: not found")

// Store is the aggregate repository handle. cmd/orion/main.go
// constructs it once and threads it into the rest of the service.
type Store struct {
	pool *pgxpool.Pool
}

// Open opens a pgx pool and pings to confirm the database is reachable.
func Open(ctx context.Context, dsn string) (*Store, error) {
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
	return &Store{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Pool exposes the underlying pgxpool. Used sparingly — most callers
// reach for typed methods on the store, but adapters/pg-listen needs
// a raw connection acquire.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping is wired into /api/v1/ready.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// noRow normalises pgx's ErrNoRows into our ErrNotFound.
func noRow(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
