//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZabLaboratory/Orion/internal/secretbox"
	"github.com/ZabLaboratory/Orion/internal/store"
)

const migration0007 = "../../migrations/0007_service_token_state.sql"

// randomKeyB64 mints a throwaway 32-byte key in the ORION_ENCRYPTION_KEY form.
// Tests never read the real key from the environment.
func randomKeyB64(t *testing.T) string {
	t.Helper()
	raw := make([]byte, secretbox.KeyBytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// The durable refresh token round-trips through Postgres, and a RAW read of
// service_token_state.refresh_token_enc contains no substring of the plaintext
// token (ADR ZabAuth 003 Am.3 § A3.3 part 2, RC 42 first criterion).
func TestServiceTokenState_PersistsEncrypted(t *testing.T) {
	ctx := context.Background()
	st := requireDB(t)

	if _, err := st.GetServiceRefreshToken(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get on an empty table: err = %v, want ErrNotFound", err)
	}

	box, err := secretbox.New(randomKeyB64(t))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	const token = "zab-refresh-PLAINTEXT-marker-0123456789"
	blob, err := box.Seal([]byte(token))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := st.PutServiceRefreshToken(ctx, blob); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Raw column read — bypasses the store surface entirely.
	var raw []byte
	if err := st.Pool().QueryRow(ctx,
		`SELECT refresh_token_enc FROM service_token_state WHERE id = 1`).Scan(&raw); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, []byte("PLAINTEXT-marker")) {
		t.Fatal("stored column contains the plaintext token")
	}

	got, err := st.GetServiceRefreshToken(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	plain, err := box.Open(got)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(plain) != token {
		t.Fatalf("round-trip = %q, want %q", plain, token)
	}

	// A rotation replaces the single row — it never accumulates.
	blob2, err := box.Seal([]byte(token + "-rotated"))
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	if err := st.PutServiceRefreshToken(ctx, blob2); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	var rows int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM service_token_state`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("row count = %d, want 1 (singleton)", rows)
	}
	got2, err := st.GetServiceRefreshToken(ctx)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	plain2, err := box.Open(got2)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	if string(plain2) != token+"-rotated" {
		t.Fatalf("after rotation = %q, want the rotated value", plain2)
	}

	// Re-persisting the SAME ciphertext is idempotent (the unpersisted-state
	// retry of § A3.3 part 5 leans on this).
	if err := st.PutServiceRefreshToken(ctx, blob2); err != nil {
		t.Fatalf("Put idempotent: %v", err)
	}
}

// A blob written under one key does not decrypt under another, and the store
// hands back the ciphertext untouched — the failure is loud, at the crypto
// layer, with nothing plaintext-shaped in between (RC 42 second criterion, at
// the persisted level).
func TestServiceTokenState_WrongKeyFailsAfterRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := requireDB(t)

	box, err := secretbox.New(randomKeyB64(t))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	other, err := secretbox.New(randomKeyB64(t))
	if err != nil {
		t.Fatalf("secretbox.New other: %v", err)
	}
	blob, err := box.Seal([]byte("durable-refresh-token"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := st.PutServiceRefreshToken(ctx, blob); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stored, err := st.GetServiceRefreshToken(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := other.Open(stored); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Fatalf("Open under a foreign key: err = %v, want ErrDecrypt", err)
	}
}

// `goose down` then `goose up` replays cleanly (RC 42 fourth criterion): the
// migration's Down drops the table and the Up recreates it, so a rollback and a
// re-deploy leave a usable schema.
func TestServiceTokenState_MigrationDownUp(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("ORION_E2E_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORION_E2E_DATABASE_URL not set; skipping e2e")
	}
	schema := "e2e_" + stripDashes(uuid.NewString())
	provisionSchema(t, dsn, schema)

	conn, err := pgx.Connect(ctx, dsnWithSearchPath(dsn, schema))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })

	up, down := gooseSections(t, migration0007)
	for i, step := range []struct {
		label string
		sql   string
	}{
		{"up", up}, {"down", down}, {"up again", up}, {"down again", down},
	} {
		if _, err := conn.Exec(ctx, step.sql); err != nil {
			t.Fatalf("step %d (%s): %v", i, step.label, err)
		}
	}

	// After the final down the table is gone — the migration is reversible, not
	// a one-way door.
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT to_regclass($1::text || '.service_token_state') IS NOT NULL`, schema).Scan(&exists); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	if exists {
		t.Fatal("service_token_state still present after goose down")
	}
}

// gooseSections splits a migration file into its Up and Down bodies, dropping
// the `-- +goose` marker lines so the SQL can be exec'd directly.
func gooseSections(t *testing.T, path string) (up, down string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	parts := strings.SplitN(string(body), "-- +goose Down", 2)
	if len(parts) != 2 {
		t.Fatalf("%s has no `-- +goose Down` section", path)
	}
	return dropGooseMarkers(parts[0]), dropGooseMarkers(parts[1])
}

func dropGooseMarkers(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "-- +goose") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
