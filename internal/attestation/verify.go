// Package attestation verifies the `zabcanvas.resolved-scene-ref.v1` JWS
// (ADR-BLUE-012 §6.2, Blue repo). This is the sole gate Orion checks before
// any Canvas I/O or preview/on-air mutation (ADR-BLUE-012 §4.4 step 3):
// ZabCanvas is the only emitter, Prism relays the compact JWS opaque and
// unmutated, and Orion trusts nothing else about scene selection.
package attestation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Action is one of the two operations a ResolvedSceneRef can authorize.
type Action string

const (
	ActionPreparePreview Action = "prepare-preview"
	ActionTakeOnAir      Action = "take-on-air"
)

// Claims is the single parsed object produced by Verify. Per §6.2 it is
// reused verbatim for authorization, routing and digest checks — Orion
// never reparses the raw JSON after validation.
type Claims struct {
	SchemaVersion          string   `json:"schema_version"`
	RefID                  string   `json:"ref_id"`
	AttestationID          string   `json:"attestation_id"`
	Issuer                 string   `json:"issuer"`
	Audience               string   `json:"audience"`
	Subject                string   `json:"subject"`
	OwnerID                string   `json:"owner_id"`
	TenantID               string   `json:"tenant_id"`
	ShowID                 string   `json:"show_id,omitempty"`
	StreamID               string   `json:"stream_id"`
	AllowedActions         []string `json:"allowed_actions"`
	SceneID                string   `json:"scene_id"`
	RevisionID             string   `json:"revision_id"`
	SceneDigest            string   `json:"scene_digest"`
	ArtifactSetDigest      string   `json:"artifact_set_digest"`
	BlueProgramDigest      string   `json:"blue_program_digest"`
	ReadinessAttestationID string   `json:"readiness_attestation_id"`
	ReadinessDigest        string   `json:"readiness_digest"`
	ReadinessExpiresAt     int64    `json:"readiness_expires_at"`
	IssuedAt               int64    `json:"issued_at"`
	NotBefore              int64    `json:"not_before"`
	ExpiresAt              int64    `json:"expires_at"`
	Kid                    string   `json:"-"`
	CanvasLocator          string   `json:"canvas_locator"`
}

// TrustSet resolves a protected-header `kid` to its Ed25519 public key.
// Callers pin this from ZabCanvas's published/rotated key set; an unknown
// or retired kid must be absent (or removed) from the map — Verify never
// fetches a key from a URL carried by the attestation itself (§6.2).
type TrustSet map[string]ed25519.PublicKey

// Options binds the request-side authority (the principal ZabGate injected,
// and the intent being authorized) against the signed claims. Every field
// is required except ShowID, which is only compared when non-empty.
type Options struct {
	Principal     string
	OwnerID       string
	TenantID      string
	ShowID        string
	StreamID      string
	Action        Action
	LocatorPrefix string
	Now           time.Time // zero = time.Now()
}

var (
	ErrMalformed        = errors.New("attestation: malformed JWS compact serialization")
	ErrHeader           = errors.New("attestation: invalid or non-conformant protected header")
	ErrUnknownKey       = errors.New("attestation: unknown or untrusted kid")
	ErrBadSignature     = errors.New("attestation: signature verification failed")
	ErrPayload          = errors.New("attestation: payload violates the §6.2 strict profile")
	ErrExpired          = errors.New("attestation: expired or not yet valid")
	ErrReadinessExpired = errors.New("attestation: readiness attestation expired")
	ErrIssuerAudience   = errors.New("attestation: issuer/audience mismatch")
	ErrLocator          = errors.New("attestation: canvas_locator rejected")
	ErrMismatch         = errors.New("attestation: claims do not authorize the requested scope/action")
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// requiredIssuer/requiredAudience/requiredAlg/requiredTyp are pinned per
// §6.2 — not configurable, since a divergent issuer/audience/alg/typ is
// exactly the confused-deputy this contract exists to close.
const (
	requiredIssuer   = "https://zabcanvas.internal"
	requiredAudience = "orion"
	requiredAlg      = "EdDSA"
	requiredTyp      = "zabcanvas-resolved-scene-ref+jws"
)

// Verify checks a compact JWS against trust and opts, and returns the
// single parsed Claims object on success. Any failure is fail-closed: no
// partial claims are ever returned alongside an error.
func Verify(jws string, trust TrustSet, opts Options) (*Claims, error) {
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]

	headerRaw, err := base64.RawURLEncoding.DecodeString(headerB64)
	if err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrMalformed, err)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, fmt.Errorf("%w: payload: %v", ErrMalformed, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrMalformed, err)
	}

	if err := verifyHeader(headerRaw); err != nil {
		return nil, err
	}
	kid, err := headerKid(headerRaw)
	if err != nil {
		return nil, err
	}

	pub, ok := trust[kid]
	if !ok || len(pub) == 0 {
		return nil, ErrUnknownKey
	}

	signed := []byte(headerB64 + "." + payloadB64)
	if !ed25519.Verify(pub, signed, sig) {
		return nil, ErrBadSignature
	}

	if err := rejectDuplicateKeys(payloadRaw); err != nil {
		return nil, err
	}
	if !utf8.Valid(payloadRaw) {
		return nil, fmt.Errorf("%w: invalid UTF-8", ErrPayload)
	}

	var claims Claims
	dec := json.NewDecoder(bytes.NewReader(payloadRaw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&claims); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPayload, err)
	}
	claims.Kid = kid

	if err := validateClaims(&claims); err != nil {
		return nil, err
	}

	if claims.Issuer != requiredIssuer || claims.Audience != requiredAudience {
		return nil, ErrIssuerAudience
	}

	if err := validateLocator(claims.CanvasLocator, opts.LocatorPrefix); err != nil {
		return nil, err
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	nowEpoch := now.Unix()
	if nowEpoch < claims.NotBefore || nowEpoch >= claims.ExpiresAt {
		return nil, ErrExpired
	}
	if nowEpoch >= claims.ReadinessExpiresAt {
		return nil, ErrReadinessExpired
	}

	if err := authorize(&claims, opts); err != nil {
		return nil, err
	}

	return &claims, nil
}

func verifyHeader(raw []byte) error {
	if err := rejectDuplicateKeys(raw); err != nil {
		return err
	}
	var h map[string]json.RawMessage
	if err := json.Unmarshal(raw, &h); err != nil {
		return fmt.Errorf("%w: %v", ErrHeader, err)
	}
	if _, hasCrit := h["crit"]; hasCrit {
		return fmt.Errorf("%w: crit present", ErrHeader)
	}
	// Exactly {alg, kid, typ} — no unprotected/extra parameter admitted.
	allowed := map[string]bool{"alg": true, "kid": true, "typ": true}
	for k := range h {
		if !allowed[k] {
			return fmt.Errorf("%w: unexpected header parameter %q", ErrHeader, k)
		}
	}
	var alg, typ string
	if err := unmarshalString(h["alg"], &alg); err != nil || alg != requiredAlg {
		return fmt.Errorf("%w: alg", ErrHeader)
	}
	if err := unmarshalString(h["typ"], &typ); err != nil || typ != requiredTyp {
		return fmt.Errorf("%w: typ", ErrHeader)
	}
	kidRaw, ok := h["kid"]
	if !ok {
		return fmt.Errorf("%w: missing kid", ErrHeader)
	}
	var kid string
	if err := unmarshalString(kidRaw, &kid); err != nil || kid == "" {
		return fmt.Errorf("%w: empty kid", ErrHeader)
	}
	return nil
}

func headerKid(raw []byte) (string, error) {
	var h struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", fmt.Errorf("%w: %v", ErrHeader, err)
	}
	return h.Kid, nil
}

func unmarshalString(raw json.RawMessage, out *string) error {
	if raw == nil {
		return errors.New("missing")
	}
	return json.Unmarshal(raw, out)
}

// rejectDuplicateKeys walks the top-level JSON object once and errors on a
// repeated key — encoding/json silently keeps the last value on a
// duplicate, which the §6.2 profile explicitly forbids.
func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPayload, err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return fmt.Errorf("%w: not a JSON object", ErrPayload)
	}
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrPayload, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("%w: non-string key", ErrPayload)
		}
		if seen[key] {
			return fmt.Errorf("%w: duplicate key %q", ErrPayload, key)
		}
		seen[key] = true
		// Skip the value without a nested duplicate-key pass: the §6.2
		// claim set is flat, so nested objects/arrays never carry their own
		// security-relevant keys.
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("%w: %v", ErrPayload, err)
		}
	}
	return nil
}

func validateClaims(c *Claims) error {
	// blue_program_digest is deliberately NOT in this required set
	// (ORION-NOBLUE-AND-VERSION-ALIGN, #398): a scene without a Blue
	// program is signed and airable — ZabCanvas mints such a ref with an
	// EMPTY blue_program_digest (and scene_digest == artifact_set_digest ==
	// sha256 of the LSML bundle). This is the ONLY relaxation: a non-empty
	// blue_program_digest must still be a well-formed digest (checked
	// below, all-or-nothing), and scene_digest / artifact_set_digest stay
	// required and well-formed in both cases.
	required := map[string]string{
		"schema_version":           c.SchemaVersion,
		"ref_id":                   c.RefID,
		"attestation_id":           c.AttestationID,
		"issuer":                   c.Issuer,
		"audience":                 c.Audience,
		"subject":                  c.Subject,
		"owner_id":                 c.OwnerID,
		"tenant_id":                c.TenantID,
		"stream_id":                c.StreamID,
		"scene_id":                 c.SceneID,
		"revision_id":              c.RevisionID,
		"scene_digest":             c.SceneDigest,
		"artifact_set_digest":      c.ArtifactSetDigest,
		"readiness_attestation_id": c.ReadinessAttestationID,
		"readiness_digest":         c.ReadinessDigest,
		"canvas_locator":           c.CanvasLocator,
	}
	for name, v := range required {
		if v == "" {
			return fmt.Errorf("%w: missing %s", ErrPayload, name)
		}
	}
	if c.ReadinessExpiresAt == 0 || c.IssuedAt == 0 || c.NotBefore == 0 || c.ExpiresAt == 0 {
		return fmt.Errorf("%w: missing timestamp", ErrPayload)
	}

	idFields := map[string]string{
		"ref_id":         c.RefID,
		"attestation_id": c.AttestationID,
		"scene_id":       c.SceneID,
		"revision_id":    c.RevisionID,
		"owner_id":       c.OwnerID,
		"tenant_id":      c.TenantID,
		"stream_id":      c.StreamID,
	}
	if c.ShowID != "" {
		idFields["show_id"] = c.ShowID
	}
	for name, v := range idFields {
		if !idPattern.MatchString(v) || !isNFC(v) {
			return fmt.Errorf("%w: invalid id %s", ErrPayload, name)
		}
	}

	for _, d := range []string{c.SceneDigest, c.ArtifactSetDigest, c.ReadinessDigest} {
		if !digestPattern.MatchString(d) {
			return fmt.Errorf("%w: invalid digest %q", ErrPayload, d)
		}
	}
	// All-or-nothing: empty means "no program" (accepted); anything
	// non-empty must be a well-formed digest — a malformed program digest
	// is never silently read as "no program".
	if c.BlueProgramDigest != "" && !digestPattern.MatchString(c.BlueProgramDigest) {
		return fmt.Errorf("%w: invalid digest %q", ErrPayload, c.BlueProgramDigest)
	}

	if len(c.AllowedActions) == 0 {
		return fmt.Errorf("%w: empty allowed_actions", ErrPayload)
	}
	seen := map[string]bool{}
	sorted := append([]string(nil), c.AllowedActions...)
	sort.Strings(sorted)
	for i, a := range c.AllowedActions {
		if seen[a] {
			return fmt.Errorf("%w: duplicate allowed_action %q", ErrPayload, a)
		}
		seen[a] = true
		if a != sorted[i] {
			return fmt.Errorf("%w: allowed_actions not lexically ordered", ErrPayload)
		}
	}

	return nil
}

func isNFC(s string) bool {
	return norm.NFC.IsNormalString(s)
}

// validateLocator enforces §6.2: a relative, normalized path confined
// under prefix — no scheme, host, userinfo, fragment, port, backslash,
// percent-encoding ambiguity or traversal.
func validateLocator(locator, prefix string) error {
	if locator == "" {
		return fmt.Errorf("%w: empty", ErrLocator)
	}
	if strings.ContainsAny(locator, "\\") {
		return fmt.Errorf("%w: backslash", ErrLocator)
	}
	if strings.Contains(locator, "://") || strings.HasPrefix(locator, "//") {
		return fmt.Errorf("%w: absolute/scheme-relative", ErrLocator)
	}
	if strings.ContainsAny(locator, "#@") {
		return fmt.Errorf("%w: fragment/userinfo", ErrLocator)
	}
	if strings.HasPrefix(locator, "/") {
		// A leading slash alone is not a host, but this profile confines
		// every locator under prefix — reject outright to stay fail-closed
		// rather than reasoning about host-relative edge cases.
		return fmt.Errorf("%w: absolute path", ErrLocator)
	}
	if !strings.HasPrefix(locator, prefix) {
		return fmt.Errorf("%w: outside configured prefix", ErrLocator)
	}
	for _, seg := range strings.Split(locator, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("%w: traversal segment", ErrLocator)
		}
	}
	return nil
}

func authorize(c *Claims, opts Options) error {
	if c.Subject != opts.Principal {
		return fmt.Errorf("%w: subject", ErrMismatch)
	}
	if c.OwnerID != opts.OwnerID || c.TenantID != opts.TenantID {
		return fmt.Errorf("%w: owner/tenant", ErrMismatch)
	}
	if c.StreamID != opts.StreamID {
		return fmt.Errorf("%w: stream_id", ErrMismatch)
	}
	if opts.ShowID != "" && c.ShowID != opts.ShowID {
		return fmt.Errorf("%w: show_id", ErrMismatch)
	}
	found := false
	for _, a := range c.AllowedActions {
		if Action(a) == opts.Action {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: action %q not allowed", ErrMismatch, opts.Action)
	}
	return nil
}
