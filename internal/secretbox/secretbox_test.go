package secretbox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func freshKey(t *testing.T) (string, []byte) {
	t.Helper()
	raw := make([]byte, KeyBytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw), raw
}

// A sealed token round-trips, and the ciphertext carries no substring of the
// plaintext (RC 42, first criterion — at the primitive level).
func TestSealOpen_RoundTripAndOpacity(t *testing.T) {
	keyB64, _ := freshKey(t)
	box, err := New(keyB64)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token := []byte("eyJhbGciOiJIUzI1NiJ9.refresh-token-plaintext.signature")

	blob, err := box.Seal(token)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(blob, token) {
		t.Fatal("ciphertext contains the plaintext token")
	}
	for _, frag := range [][]byte{[]byte("refresh-token-plaintext"), []byte("eyJhbGciOiJIUzI1NiJ9")} {
		if bytes.Contains(blob, frag) {
			t.Fatalf("ciphertext contains plaintext fragment %q", frag)
		}
	}
	if len(blob) != NonceBytes+len(token)+16 {
		t.Fatalf("blob length = %d, want nonce(%d) + plaintext(%d) + tag(16)", len(blob), NonceBytes, len(token))
	}

	got, err := box.Open(blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, token) {
		t.Fatalf("Open = %q, want %q", got, token)
	}
}

// Every write draws a fresh nonce, so the same plaintext seals to different
// bytes and the stored nonce prefix never repeats.
func TestSeal_FreshNoncePerWrite(t *testing.T) {
	keyB64, _ := freshKey(t)
	box, _ := New(keyB64)
	token := []byte("same-token")

	a, err := box.Seal(token)
	if err != nil {
		t.Fatalf("Seal a: %v", err)
	}
	b, err := box.Seal(token)
	if err != nil {
		t.Fatalf("Seal b: %v", err)
	}
	if bytes.Equal(a[:NonceBytes], b[:NonceBytes]) {
		t.Fatal("nonce reused across two writes")
	}
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext produced identical blobs")
	}
}

// Decryption under a different key fails loudly — no plaintext fallback
// (RC 42, second criterion).
func TestOpen_WrongKeyFailsLoudly(t *testing.T) {
	keyA, _ := freshKey(t)
	keyB, _ := freshKey(t)
	boxA, _ := New(keyA)
	boxB, _ := New(keyB)

	blob, err := boxA.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := boxB.Open(blob)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Open under wrong key: err = %v, want ErrDecrypt", err)
	}
	if got != nil {
		t.Fatalf("Open under wrong key returned %q, want nil", got)
	}
}

// A ciphertext whose additional data does not match AAD is rejected, so a blob
// encrypted for another purpose (or another service) cannot be swapped in
// (RC 42, third criterion).
func TestOpen_MismatchedAdditionalDataRejected(t *testing.T) {
	keyB64, raw := freshKey(t)
	box, _ := New(keyB64)

	// Same key, same primitive, different additional data.
	block, err := aes.NewCipher(raw)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand: %v", err)
	}
	foreign := aead.Seal(nonce, nonce, []byte("secret"), []byte("quasar.some_other_purpose.v1"))

	if _, err := box.Open(foreign); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Open with foreign AAD: err = %v, want ErrDecrypt", err)
	}

	// Sanity: the very same bytes under the right AAD do open.
	mine := aead.Seal(nil, nonce, []byte("secret"), []byte(AAD))
	if _, err := box.Open(append(append([]byte{}, nonce...), mine...)); err != nil {
		t.Fatalf("Open with correct AAD: %v", err)
	}
}

// A flipped byte anywhere in the blob fails authentication.
func TestOpen_TamperedBlobRejected(t *testing.T) {
	keyB64, _ := freshKey(t)
	box, _ := New(keyB64)
	blob, err := box.Seal([]byte("secret-token-value"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	for _, i := range []int{0, NonceBytes, len(blob) - 1} {
		tampered := append([]byte{}, blob...)
		tampered[i] ^= 0x01
		if _, err := box.Open(tampered); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("Open tampered at %d: err = %v, want ErrDecrypt", i, err)
		}
	}
}

func TestOpen_TooShortBlob(t *testing.T) {
	keyB64, _ := freshKey(t)
	box, _ := New(keyB64)
	for _, blob := range [][]byte{nil, {}, make([]byte, NonceBytes), make([]byte, NonceBytes+15)} {
		if _, err := box.Open(blob); !errors.Is(err, ErrMalformedCiphertext) {
			t.Fatalf("Open(len %d): err = %v, want ErrMalformedCiphertext", len(blob), err)
		}
	}
}

// A malformed or wrong-length key is a refusal to arm, not a degraded mode.
func TestNew_MalformedKeyRefusesToArm(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	long := base64.StdEncoding.EncodeToString(make([]byte, 64))
	cases := map[string]string{
		"empty":        "",
		"not base64":   "not-base64-!!!",
		"16 bytes":     short,
		"64 bytes":     long,
		"raw 32 bytes": string(make([]byte, KeyBytes)), // the key, unencoded
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			box, err := New(key)
			if !errors.Is(err, ErrMalformedKey) {
				t.Fatalf("New(%s): err = %v, want ErrMalformedKey", name, err)
			}
			if box != nil {
				t.Fatalf("New(%s) returned a usable box on a bad key", name)
			}
		})
	}
}
