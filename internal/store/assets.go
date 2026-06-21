package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Asset is the metadata row for a content-addressed asset. The binary
// lives at filesystem_path under cfg.AssetRoot.
type Asset struct {
	ID             uuid.UUID
	SHA256Hex      string
	Mime           string
	SizeBytes      int64
	FilesystemPath string
	CreatedAt      time.Time
}

// PutAsset is upsert-by-content-hash. Two distinct uploads of the
// same bytes produce a single row; the second call returns the
// existing row's metadata. The ID stays stable across re-uploads
// because the content hash determines it (callers derive the UUIDv5
// from sha256_hex at the asset-handling layer).
func (s *PGStore) PutAsset(ctx context.Context, a Asset) (*Asset, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO assets (id, sha256_hex, mime, size_bytes, filesystem_path)
		   VALUES ($1, $2, $3, $4, $5)
		   ON CONFLICT (sha256_hex) DO UPDATE SET mime = EXCLUDED.mime
		   RETURNING id, sha256_hex, mime, size_bytes, filesystem_path, created_at`,
		a.ID, a.SHA256Hex, a.Mime, a.SizeBytes, a.FilesystemPath,
	)
	var out Asset
	err := row.Scan(&out.ID, &out.SHA256Hex, &out.Mime, &out.SizeBytes,
		&out.FilesystemPath, &out.CreatedAt)
	if err != nil {
		return nil, noRow(err)
	}
	return &out, nil
}

// GetAsset fetches by id (the caller has validated it as a uuid).
func (s *PGStore) GetAsset(ctx context.Context, id uuid.UUID) (*Asset, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, sha256_hex, mime, size_bytes, filesystem_path, created_at
		   FROM assets WHERE id = $1`, id,
	)
	var a Asset
	err := row.Scan(&a.ID, &a.SHA256Hex, &a.Mime, &a.SizeBytes,
		&a.FilesystemPath, &a.CreatedAt)
	if err != nil {
		return nil, noRow(err)
	}
	return &a, nil
}
