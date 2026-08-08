// Package secretbox is Orion's encryption-at-rest primitive for the durable
// service refresh token (ADR ZabAuth 003 Amendment 3 § A3.3 part 2).
//
// § 3.4 of that ADR enacts a property — a credential is encrypted at rest under
// a key owned by that service alone — and names Fernet only because every
// client it had was Python. Go has no Fernet in stdlib and the available port is
// an unmaintained third-party dependency, so Orion satisfies the same property
// with AES-256-GCM from `crypto/aes` + `crypto/cipher`: strictly no weaker than
// Fernet (AES-128-CBC + HMAC-SHA256), zero new dependency.
//
// Wire format: `nonce || ciphertext||tag`, with a fresh 12-byte random nonce per
// write. The AEAD additional data is the fixed literal AAD below, binding a
// ciphertext to its purpose so a blob cannot be swapped in from elsewhere.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// AAD is the AEAD additional data bound into every service-refresh-token
// ciphertext. It is versioned: a future format change takes a new literal
// rather than silently reinterpreting old blobs.
const AAD = "orion.service_refresh_token.v1"

// KeyBytes is the required decoded length of ORION_ENCRYPTION_KEY (AES-256).
const KeyBytes = 32

// NonceBytes is the GCM standard nonce length used for every write.
const NonceBytes = 12

// ErrMalformedKey is returned when ORION_ENCRYPTION_KEY is absent, not valid
// base64, or not exactly KeyBytes once decoded. Callers must treat it as a
// refusal to arm — never as a licence to write plaintext.
var ErrMalformedKey = errors.New("secretbox: malformed encryption key")

// ErrMalformedCiphertext is returned when a stored blob is too short to carry a
// nonce and a tag. Authentication failures surface as ErrDecrypt.
var ErrMalformedCiphertext = errors.New("secretbox: malformed ciphertext")

// ErrDecrypt is returned when a blob fails AEAD authentication: wrong key,
// tampered bytes, or additional data that does not match AAD. It is
// deliberately opaque — the three cases are indistinguishable by design.
var ErrDecrypt = errors.New("secretbox: decrypt failed")

// Box seals and opens the durable service refresh token under one key.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from the base64 (standard encoding, padded) form of a
// 32-byte key — the ORION_ENCRYPTION_KEY contract. A malformed or wrong-length
// key is ErrMalformedKey: there is no degraded mode, the caller refuses to arm.
//
// The key must be freshly generated and distinct from QUASAR_ENCRYPTION_KEY,
// COSMOS_ENCRYPTION_KEY and BLUE_ENCRYPTION_KEY — a service-owned key never
// crosses a service boundary (§ A3.3 part 2). That is a provisioning rule; it
// cannot be checked here, since Orion never sees the other three.
func New(keyB64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("%w: not base64", ErrMalformedKey)
	}
	if len(key) != KeyBytes {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrMalformedKey, len(key), KeyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedKey, err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext, returning `nonce || ciphertext||tag`. The nonce is
// fresh random bytes on every call, so two seals of the same plaintext differ.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, []byte(AAD)), nil
}

// Open authenticates and decrypts a blob produced by Seal. A wrong key, a
// tampered byte or mismatched additional data all fail here — loudly, with no
// plaintext fallback.
func (b *Box) Open(blob []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(blob) < ns+b.aead.Overhead() {
		return nil, ErrMalformedCiphertext
	}
	plaintext, err := b.aead.Open(nil, blob[:ns], blob[ns:], []byte(AAD))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
