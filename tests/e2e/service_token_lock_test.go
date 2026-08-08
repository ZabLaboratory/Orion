//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/store"
)

// requireDSN returns the e2e DSN or skips. The advisory-lock tests need the
// raw DSN rather than a Store: the lock deliberately rides its OWN connection,
// outside the pool.
func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ORION_E2E_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORION_E2E_DATABASE_URL not set; skipping e2e")
	}
	return dsn
}

// RC 44, against a real Postgres: the FIRST process takes the rotation lock,
// the SECOND does not get it — and is told so without an error, because a lost
// lock is a degradation, not a failure (§ A3.4 (f)).
//
// The two ProcessLocks here stand in for two processes: each opens its own
// connection, which is precisely what makes the exclusion real rather than
// in-process bookkeeping.
func TestServiceTokenLock_SecondProcessIsRefused(t *testing.T) {
	ctx := context.Background()
	dsn := requireDSN(t)
	key := store.ServiceTokenLockKey()

	first, held, err := store.TryAcquireProcessLock(ctx, dsn, key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !held {
		t.Fatal("first process did not get the lock on a free database")
	}

	second, held2, err2 := store.TryAcquireProcessLock(ctx, dsn, key)
	if err2 != nil {
		t.Fatalf("the second acquire must not error, only report not-held: %v", err2)
	}
	if held2 {
		second.Release()
		first.Release()
		t.Fatal("two processes hold the rotation lock simultaneously")
	}
	if second != nil {
		t.Fatal("a refused acquire returned a lock handle")
	}

	// Releasing the first hands the lock over — the invariant is one holder at
	// a time, not one holder forever. This is also the restart path: a process
	// that dies frees the lock with its session.
	first.Release()
	third, held3, err3 := store.TryAcquireProcessLock(ctx, dsn, key)
	if err3 != nil {
		t.Fatalf("third acquire: %v", err3)
	}
	if !held3 {
		t.Fatal("the lock was not released when its session closed")
	}
	third.Release()
}

// A different key is a different lock: the exclusion is scoped to the rotation
// invariant, it does not accidentally serialise unrelated work on the database.
func TestServiceTokenLock_ScopedToItsKey(t *testing.T) {
	ctx := context.Background()
	dsn := requireDSN(t)

	a, heldA, err := store.TryAcquireProcessLock(ctx, dsn, store.ServiceTokenLockKey())
	if err != nil || !heldA {
		t.Fatalf("acquire rotation lock: held=%v err=%v", heldA, err)
	}
	defer a.Release()

	b, heldB, err := store.TryAcquireProcessLock(ctx, dsn, store.ServiceTokenLockKey()+1)
	if err != nil {
		t.Fatalf("acquire neighbouring key: %v", err)
	}
	if !heldB {
		t.Fatal("an unrelated advisory key was blocked by the rotation lock")
	}
	b.Release()
}
