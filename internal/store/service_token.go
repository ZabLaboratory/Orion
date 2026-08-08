package store

import (
	"context"
	"errors"
	"fmt"
)

// ErrDurableServiceTokenUnsupported is returned by the SQLite backend for both
// durable service-token methods (ADR ZabAuth 003 Amendment 3 § A3.3 part 3).
// The durable model is antenne-only: an embedded-local sidecar that persisted
// the prod refresh token would rotate the antenne's family into `reuse` and
// revoke it — the show goes down from a laptop. A caller that forgets the
// profile branch must fail loudly here, never succeed silently.
var ErrDurableServiceTokenUnsupported = errors.New("store: durable service token unsupported in this profile")

// GetServiceRefreshToken reads the encrypted durable refresh token
// (migrations/0007, singleton row id = 1). The returned bytes are the opaque
// `nonce || ciphertext||tag` blob — the store never holds the key and never
// sees plaintext. ErrNotFound when no credential has been persisted yet.
func (s *PGStore) GetServiceRefreshToken(ctx context.Context) ([]byte, error) {
	var enc []byte
	err := s.pool.QueryRow(ctx,
		`SELECT refresh_token_enc FROM service_token_state WHERE id = 1`,
	).Scan(&enc)
	if err != nil {
		return nil, noRow(err)
	}
	if len(enc) == 0 {
		// NOT NULL cannot rule out a zero-length blob; treat it as absence
		// rather than handing the caller something that cannot decrypt.
		return nil, ErrNotFound
	}
	return enc, nil
}

// PutServiceRefreshToken persists the encrypted durable refresh token,
// replacing whatever was there and stamping rotated_at. Writing the same value
// twice is idempotent (single-row upsert, not a rotation), which is what makes
// the unpersisted-state retry safe (§ A3.3 part 5).
//
// The caller passes ciphertext. Passing plaintext here is a programming error
// the store cannot detect, so the encryption lives one layer up, in the only
// component that holds the key.
func (s *PGStore) PutServiceRefreshToken(ctx context.Context, enc []byte) error {
	if len(enc) == 0 {
		return fmt.Errorf("put service refresh token: empty ciphertext")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO service_token_state (id, refresh_token_enc, rotated_at)
		      VALUES (1, $1, now())
		 ON CONFLICT (id) DO UPDATE SET refresh_token_enc = EXCLUDED.refresh_token_enc,
		                                rotated_at        = now()`,
		enc,
	)
	if err != nil {
		return fmt.Errorf("put service refresh token: %w", err)
	}
	return nil
}

// GetServiceRefreshToken is unsupported on SQLite: see
// ErrDurableServiceTokenUnsupported.
func (s *SQLiteStore) GetServiceRefreshToken(_ context.Context) ([]byte, error) {
	return nil, ErrDurableServiceTokenUnsupported
}

// PutServiceRefreshToken is unsupported on SQLite: see
// ErrDurableServiceTokenUnsupported.
func (s *SQLiteStore) PutServiceRefreshToken(_ context.Context, _ []byte) error {
	return ErrDurableServiceTokenUnsupported
}
