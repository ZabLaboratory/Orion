package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ServiceTokenLockName is the literal the durable service-token advisory lock
// key is derived from. It names the invariant, not the component: ONE process
// per database may rotate the refresh-token family
// (ADR ZabAuth 003 Amendment 3 § A3.3 part 4, § 3.4).
const ServiceTokenLockName = "orion.service_token.rotator.v1"

// ServiceTokenLockKey is the pg_try_advisory_lock key for that invariant,
// derived from ServiceTokenLockName so the number is reproducible rather than
// folklore: the first 8 bytes of its SHA-256, big-endian, as a signed int64
// (Postgres advisory-lock keys are bigint, so the sign bit is data, not error).
//
// The value is pinned by TestServiceTokenLockKey_IsPinned — changing it would
// silently let a second process rotate alongside the first, which is the exact
// failure the lock exists to prevent.
func ServiceTokenLockKey() int64 {
	sum := sha256.Sum256([]byte(ServiceTokenLockName))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// ProcessLock is a session-scoped Postgres advisory lock held on its OWN
// connection for the life of the process.
//
// The dedicated connection is the whole point. A session-scoped lock is
// released when its session ends — including when the process is killed or the
// host dies, which no application-level lease can promise. Taking it on a
// pooled connection would be wrong twice over: the pool hands that connection
// to unrelated queries while the lock rides it, and returning it to the pool
// does not end the session, so the lock would outlive any notion of ownership.
type ProcessLock struct {
	conn *pgx.Conn
	key  int64
}

// TryAcquireProcessLock opens a dedicated connection and attempts the advisory
// lock WITHOUT blocking.
//
// Three outcomes, deliberately distinct:
//
//   - (lock, true, nil)  — held; release with Release, or by dying.
//   - (nil, false, nil)  — another live process holds it. NOT an error: the
//     caller degrades, it does not fail. Orion is the antenna; it stays on air
//     without a service token rather than refusing to boot (§ A3.4 (f)).
//   - (nil, false, err)  — the database could not be reached or answered
//     nonsense. Also handled by degrading, but it deserves a different log.
func TryAcquireProcessLock(ctx context.Context, dsn string, key int64) (*ProcessLock, bool, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, false, fmt.Errorf("store: advisory lock: connect: %w", err)
	}
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&held); err != nil {
		_ = conn.Close(ctx)
		return nil, false, fmt.Errorf("store: advisory lock: %w", err)
	}
	if !held {
		_ = conn.Close(ctx)
		return nil, false, nil
	}
	return &ProcessLock{conn: conn, key: key}, true, nil
}

// Release drops the lock by closing its session. Safe on a nil receiver and
// safe to call twice, so callers can defer it unconditionally.
func (l *ProcessLock) Release() {
	if l == nil || l.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Unlocking explicitly is courtesy, not correctness — closing the session
	// releases it regardless, which is what covers the crash path.
	_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, l.key)
	_ = l.conn.Close(ctx)
	l.conn = nil
}
