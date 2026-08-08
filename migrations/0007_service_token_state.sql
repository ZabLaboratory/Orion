-- +goose Up
-- +goose StatementBegin

-- The durable service-token state (ADR ZabAuth 003 Amendment 3 § A3.3 part 2).
-- Orion's ServiceTokenManager rotates a refresh token by possession alone; the
-- current refresh token must survive a restart, or the family is lost and the
-- process falls back on a standing credential — the exact shape this ADR
-- retires.
--
-- Singleton row: id is a smallint PK pinned to 1, so there is exactly one
-- service_token_state row (one process, one family — § A3.3 part 4).
--
-- refresh_token_enc holds AES-256-GCM ciphertext (`nonce || ciphertext||tag`,
-- 12-byte random nonce per write, additional data = the fixed literal
-- `orion.service_refresh_token.v1`) under ORION_ENCRYPTION_KEY, a key owned by
-- Orion alone. The column NEVER holds plaintext: the manager refuses to arm on
-- a malformed key rather than degrade to a plaintext write.
--
-- No row is seeded: an empty table means "no durable credential yet", which the
-- store surfaces as ErrNotFound (distinct from a row holding an unreadable
-- blob, which is an error, not an absence).

CREATE TABLE service_token_state (
    id                smallint    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    refresh_token_enc bytea       NOT NULL,
    rotated_at        timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS service_token_state;
-- +goose StatementEnd
