package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Tx is the store-neutral transaction handle the transactional Store
// methods accept (ADR 016 §3.2). It exposes only the two operations the
// push/archive paths need — a write that reports rows-affected and a
// single-row read — so any backend (pgx tx, database/sql tx) can satisfy
// it without leaking a driver type into the Store surface.
//
// Before #222 the interface named pgx.Tx directly; that coupling blocked a
// second backend (the store.go comment flagged it as #222's job). Tx is
// that generalisation: *PGStore wraps a pgx.Tx in pgxTx, *sqliteStore wraps
// a *sql.Tx in sqlTx, and the SQL written in the per-table methods is
// dialect text the backend already speaks.
type Tx interface {
	// Exec runs a write and returns the number of rows affected.
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
	// QueryRow runs a single-row read; the returned Row defers errors to Scan.
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// Row is a single result row; Scan copies its columns into dest. A no-rows
// read is reported by Scan returning a backend sentinel that the per-table
// methods normalise into ErrNotFound (noRow / noRowSQL).
type Row interface {
	Scan(dest ...any) error
}

// pgxTx adapts a pgx.Tx to the store-neutral Tx interface. Exec collapses
// pgx's pgconn.CommandTag into the rows-affected count; QueryRow forwards
// pgx.Row (which already satisfies Row).
type pgxTx struct{ tx pgx.Tx }

func (t pgxTx) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (t pgxTx) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

// compile-time guard: pgx's CommandTag is the source of rows-affected.
var _ = pgconn.CommandTag{}
