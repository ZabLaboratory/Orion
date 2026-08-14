package providers

import "github.com/ZabLaboratory/Orion/internal/canonical"

// CanonicalBytes and Digest re-export internal/canonical — the shared LSML
// canonicalization algorithm blueruntime.ParseEvent/ParseCompletion/
// ParseProgram verify on receipt. Moved to its own leaf package so
// internal/bluehost can depend on it too without an import cycle through
// internal/providers (which itself imports internal/bluehost, see
// events.go). Kept here as thin re-exports so existing callers of
// providers.CanonicalBytes/providers.Digest are unaffected.

// CanonicalBytes serializes value per the shared LSML canonicalization
// profile. See internal/canonical.CanonicalBytes.
func CanonicalBytes(value any) ([]byte, error) {
	return canonical.CanonicalBytes(value)
}

// Digest returns the `sha256:<hex>` digest of value's canonical bytes. See
// internal/canonical.Digest.
func Digest(value any) (string, error) {
	return canonical.Digest(value)
}
