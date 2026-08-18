package attestation

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testKid = "canvas-key-1"

func testTrust(t *testing.T, pub ed25519.PublicKey) TrustSet {
	t.Helper()
	return TrustSet{testKid: pub}
}

type rawClaims map[string]any

func baseClaims(now time.Time) rawClaims {
	return rawClaims{
		"schema_version":           "1",
		"ref_id":                   "ref-abc123",
		"attestation_id":           "att-abc123",
		"issuer":                   requiredIssuer,
		"audience":                 requiredAudience,
		"kid":                      testKid,
		"subject":                  "principal-1",
		"owner_id":                 "owner-1",
		"tenant_id":                "tenant-1",
		"stream_id":                "stream-1",
		"allowed_actions":          []string{"prepare-preview", "take-on-air"},
		"scene_id":                 "scene-1",
		"revision_id":              "rev-1",
		"scene_digest":             "sha256:" + strings.Repeat("a", 64),
		"artifact_set_digest":      "sha256:" + strings.Repeat("b", 64),
		"blue_program_digest":      "sha256:" + strings.Repeat("c", 64),
		"readiness_attestation_id": "ready-1",
		"readiness_digest":         "sha256:" + strings.Repeat("d", 64),
		"readiness_expires_at":     now.Add(time.Hour).Unix(),
		"issued_at":                now.Unix(),
		"not_before":               now.Add(-time.Minute).Unix(),
		"expires_at":               now.Add(time.Hour).Unix(),
		"canvas_locator":           "scenes/scene-1/revisions/rev-1",
	}
}

func sign(t *testing.T, priv ed25519.PrivateKey, header, payload rawClaims) string {
	t.Helper()
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	h64 := base64.RawURLEncoding.EncodeToString(hb)
	p64 := base64.RawURLEncoding.EncodeToString(pb)
	sig := ed25519.Sign(priv, []byte(h64+"."+p64))
	s64 := base64.RawURLEncoding.EncodeToString(sig)
	return h64 + "." + p64 + "." + s64
}

func validOptions(now time.Time) Options {
	return Options{
		Principal:     "principal-1",
		OwnerID:       "owner-1",
		TenantID:      "tenant-1",
		StreamID:      "stream-1",
		Action:        ActionPreparePreview,
		LocatorPrefix: "scenes/",
		Now:           now,
	}
}

func validHeader() rawClaims {
	return rawClaims{"alg": requiredAlg, "kid": testKid, "typ": requiredTyp}
}

func TestVerify_Valid(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	jws := sign(t, priv, validHeader(), baseClaims(now))

	claims, err := Verify(jws, testTrust(t, pub), validOptions(now))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.SceneID != "scene-1" {
		t.Fatalf("unexpected scene_id: %q", claims.SceneID)
	}
	if claims.Kid != testKid {
		t.Fatalf("unexpected kid: %q", claims.Kid)
	}
}

func TestVerify_RejectsTamperedPayload(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	jws := sign(t, priv, validHeader(), baseClaims(now))
	parts := strings.Split(jws, ".")
	tampered := strings.Split(sign(t, priv, validHeader(), func() rawClaims {
		c := baseClaims(now)
		c["scene_id"] = "scene-EVIL"
		return c
	}()), ".")
	forged := parts[0] + "." + tampered[1] + "." + parts[2]

	if _, err := Verify(forged, testTrust(t, pub), validOptions(now)); err != ErrBadSignature {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}
}

func TestVerify_UnknownKid(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	jws := sign(t, priv, validHeader(), baseClaims(now))
	if _, err := Verify(jws, TrustSet{}, validOptions(now)); err != ErrUnknownKey {
		t.Fatalf("expected ErrUnknownKey, got %v", err)
	}
}

func TestVerify_RejectsPayloadKidMismatch(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := baseClaims(now)
	c["kid"] = "different-key"
	jws := sign(t, priv, validHeader(), c)
	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); err == nil || !strings.Contains(err.Error(), "payload kid does not match") {
		t.Fatalf("expected payload/header kid mismatch rejection, got %v", err)
	}
}

func TestVerify_RejectsAlgNone(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	h := validHeader()
	h["alg"] = "none"
	jws := sign(t, priv, h, baseClaims(now))
	_, err = Verify(jws, testTrust(t, pub), validOptions(now))
	if err == nil || !strings.Contains(err.Error(), "attestation:") {
		t.Fatalf("expected header rejection, got %v", err)
	}
}

func TestVerify_RejectsCrit(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	h := validHeader()
	h["crit"] = []string{"b64"}
	jws := sign(t, priv, h, baseClaims(now))
	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); err == nil {
		t.Fatal("expected crit rejection")
	}
}

func TestVerify_RejectsUnknownHeaderParam(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	h := validHeader()
	h["jku"] = "https://evil.example/keys"
	jws := sign(t, priv, h, baseClaims(now))
	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); err == nil {
		t.Fatal("expected unexpected-header rejection")
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := baseClaims(now)
	c["expires_at"] = now.Add(-time.Second).Unix()
	jws := sign(t, priv, validHeader(), c)
	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestVerify_RejectsExpiredReadiness(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := baseClaims(now)
	c["readiness_expires_at"] = now.Add(-time.Second).Unix()
	jws := sign(t, priv, validHeader(), c)
	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); err != ErrReadinessExpired {
		t.Fatalf("expected ErrReadinessExpired, got %v", err)
	}
}

func TestVerify_RejectsWrongAudience(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := baseClaims(now)
	c["audience"] = "someone-else"
	jws := sign(t, priv, validHeader(), c)
	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); err != ErrIssuerAudience {
		t.Fatalf("expected ErrIssuerAudience, got %v", err)
	}
}

func TestVerify_RejectsActionNotAllowed(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := baseClaims(now)
	c["allowed_actions"] = []string{"prepare-preview"}
	jws := sign(t, priv, validHeader(), c)
	opts := validOptions(now)
	opts.Action = ActionTakeOnAir
	if _, err := Verify(jws, testTrust(t, pub), opts); !errors.Is(err, ErrMismatch) {
		t.Fatalf("expected ErrMismatch, got %v", err)
	}
}

func TestVerify_RejectsPrincipalMismatch(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	jws := sign(t, priv, validHeader(), baseClaims(now))
	opts := validOptions(now)
	opts.Principal = "someone-else"
	if _, err := Verify(jws, testTrust(t, pub), opts); !errors.Is(err, ErrMismatch) {
		t.Fatalf("expected ErrMismatch, got %v", err)
	}
}

func TestVerify_RejectsLocatorTraversal(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := baseClaims(now)
	c["canvas_locator"] = "scenes/../../etc/passwd"
	jws := sign(t, priv, validHeader(), c)
	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); err == nil {
		t.Fatal("expected locator rejection")
	}
}

func TestVerify_RejectsLocatorAbsoluteURL(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := baseClaims(now)
	c["canvas_locator"] = "https://evil.example/scenes/scene-1"
	jws := sign(t, priv, validHeader(), c)
	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); err == nil {
		t.Fatal("expected locator rejection")
	}
}

func TestValidateLocator_AcceptsCanonicalCanvasAPIPath(t *testing.T) {
	if err := validateLocator(
		"/api/v1/scenes/scene-1/resolved-scene-ref/ref-1",
		"/api/v1/scenes/",
	); err != nil {
		t.Fatalf("canonical Canvas locator must be accepted: %v", err)
	}
}

func TestValidateLocator_RejectsCanonicalPathWithWrongPrefix(t *testing.T) {
	if err := validateLocator(
		"/api/v1/scenes/scene-1/resolved-scene-ref/ref-1",
		"/api/v1/admin/",
	); err == nil {
		t.Fatal("locator outside configured API prefix must be rejected")
	}
}

func TestVerify_RejectsDuplicateKeyRaw(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	hb, _ := json.Marshal(validHeader())
	h64 := base64.RawURLEncoding.EncodeToString(hb)
	// Hand-crafted payload with a duplicate "scene_id" key — Go's encoder
	// can't produce this, so it is built as a raw string.
	rawPayload := `{"schema_version":"1","ref_id":"ref-abc123","attestation_id":"att-abc123",` +
		`"issuer":"` + requiredIssuer + `","audience":"orion","subject":"principal-1",` +
		`"owner_id":"owner-1","tenant_id":"tenant-1","stream_id":"stream-1",` +
		`"allowed_actions":["prepare-preview","take-on-air"],"scene_id":"scene-1",` +
		`"scene_id":"scene-EVIL","revision_id":"rev-1","scene_digest":"sha256:` + strings.Repeat("a", 64) + `",` +
		`"artifact_set_digest":"sha256:` + strings.Repeat("b", 64) + `","blue_program_digest":"sha256:` + strings.Repeat("c", 64) + `",` +
		`"readiness_attestation_id":"ready-1","readiness_digest":"sha256:` + strings.Repeat("d", 64) + `",` +
		`"readiness_expires_at":` + itoa(now.Add(time.Hour).Unix()) + `,"issued_at":` + itoa(now.Unix()) +
		`,"not_before":` + itoa(now.Add(-time.Minute).Unix()) + `,"expires_at":` + itoa(now.Add(time.Hour).Unix()) +
		`,"canvas_locator":"scenes/scene-1/revisions/rev-1"}`
	p64 := base64.RawURLEncoding.EncodeToString([]byte(rawPayload))
	sig := ed25519.Sign(priv, []byte(h64+"."+p64))
	s64 := base64.RawURLEncoding.EncodeToString(sig)
	jws := h64 + "." + p64 + "." + s64

	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); err == nil {
		t.Fatal("expected duplicate-key rejection")
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestVerify_AcceptsEmptyBlueProgramDigest — a scene without a Blue
// program is signed and airable (ORION-NOBLUE-AND-VERSION-ALIGN, #398):
// ZabCanvas mints its ref with an EMPTY blue_program_digest. This is the
// only relaxation — every other digest stays required and well-formed.
func TestVerify_AcceptsEmptyBlueProgramDigest(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := baseClaims(now)
	c["blue_program_digest"] = ""
	jws := sign(t, priv, validHeader(), c)

	claims, err := Verify(jws, testTrust(t, pub), validOptions(now))
	if err != nil {
		t.Fatalf("Verify must accept an empty blue_program_digest (no-program scene): %v", err)
	}
	if claims.BlueProgramDigest != "" {
		t.Fatalf("expected empty BlueProgramDigest, got %q", claims.BlueProgramDigest)
	}
}

// TestVerify_RejectsMalformedBlueProgramDigest — all-or-nothing: a
// NON-empty malformed program digest is still refused, never silently
// read as "no program".
func TestVerify_RejectsMalformedBlueProgramDigest(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	c := baseClaims(now)
	c["blue_program_digest"] = "sha256:not-hex"
	jws := sign(t, priv, validHeader(), c)

	if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); !errors.Is(err, ErrPayload) {
		t.Fatalf("expected ErrPayload for a malformed non-empty blue_program_digest, got %v", err)
	}
}

// TestVerify_StillRequiresSceneAndArtifactDigests — the no-program
// relaxation must not leak to the other digests: empty scene_digest or
// artifact_set_digest stays refused.
func TestVerify_StillRequiresSceneAndArtifactDigests(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	for _, field := range []string{"scene_digest", "artifact_set_digest"} {
		c := baseClaims(now)
		c["blue_program_digest"] = ""
		c[field] = ""
		jws := sign(t, priv, validHeader(), c)
		if _, err := Verify(jws, testTrust(t, pub), validOptions(now)); !errors.Is(err, ErrPayload) {
			t.Fatalf("expected ErrPayload for empty %s, got %v", field, err)
		}
	}
}
