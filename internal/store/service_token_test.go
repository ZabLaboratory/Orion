package store

import (
	"context"
	"errors"
	"testing"
)

// The SQLite backend refuses both durable service-token methods with a typed
// error (ADR ZabAuth 003 Am.3 § A3.3 part 3). A silent success here would let
// an embedded-local sidecar persist — and later replay — the antenne's family.
func TestSQLite_DurableServiceTokenUnsupported(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(ctx, t.TempDir()+"/orion.db")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(st.Close)

	if err := st.PutServiceRefreshToken(ctx, []byte("ciphertext")); !errors.Is(err, ErrDurableServiceTokenUnsupported) {
		t.Fatalf("Put: err = %v, want ErrDurableServiceTokenUnsupported", err)
	}
	got, err := st.GetServiceRefreshToken(ctx)
	if !errors.Is(err, ErrDurableServiceTokenUnsupported) {
		t.Fatalf("Get: err = %v, want ErrDurableServiceTokenUnsupported", err)
	}
	if got != nil {
		t.Fatalf("Get returned %q on an unsupported backend", got)
	}
	// It is NOT an absence: a caller routing on ErrNotFound must not mistake
	// the refusal for "no credential yet" and proceed.
	if errors.Is(err, ErrNotFound) {
		t.Fatal("unsupported error must not satisfy errors.Is(err, ErrNotFound)")
	}
}
