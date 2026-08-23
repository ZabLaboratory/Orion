package auth

import (
	"context"
	"path/filepath"
	"testing"
)

func TestFileServiceTokenStoreRoundTrip(t *testing.T) {
	store := &fileServiceTokenStore{path: filepath.Join(t.TempDir(), "state.bin")}
	want := []byte("encrypted-refresh-state")
	if err := store.Put(context.Background(), want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("state = %q, want %q", got, want)
	}
}
