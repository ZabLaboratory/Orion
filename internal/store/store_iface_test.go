package store

import "testing"

// TestPGStoreSatisfiesStore is the RC-3 foundation guard: the Postgres
// implementation satisfies the extracted Store interface (ADR 016 §3.2).
// The real enforcement is the compile-time assertion in store.go
// (var _ Store = (*PGStore)(nil)); this test makes the contract
// explicit and gives a second backend (#222 sqliteStore) a place to
// assert the same parity. No DB is touched — it is a pure type check.
func TestPGStoreSatisfiesStore(_ *testing.T) {
	var _ Store = (*PGStore)(nil)
}
