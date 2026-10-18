package protocol

// ErrorCode is a closed taxonomy of LSDP/1 error codes. The set is
// frozen for LSDP/1.x ; new codes require a minor version bump
// (LSDP/1.1) per spec § 13.
//
// Every code has a documented recoverability semantics — see
// Recoverable() and the per-code commentary in ERROR-CODES.md.
type ErrorCode string

const (
	// CodeAuthDenied — token invalid, expired, or revoked.
	// Recoverable : false. Server closes.
	CodeAuthDenied ErrorCode = "AUTH_DENIED"

	// CodeWriteForbidden — connection role does not permit the input.
	// Recoverable : true. Connection stays open ; only the input is
	// rejected.
	CodeWriteForbidden ErrorCode = "WRITE_FORBIDDEN"

	// CodeSceneNotFound — scene field in subscribe is unknown.
	// Recoverable : false. Server closes.
	CodeSceneNotFound ErrorCode = "SCENE_NOT_FOUND"

	// CodeBundleFetchFailed — runtime cannot retrieve the LSML bundle.
	// Recoverable : true. Runtime retries with backoff (≤ 3 attempts).
	CodeBundleFetchFailed ErrorCode = "BUNDLE_FETCH_FAILED"

	// CodeBundleIncompatible — bundle declares an LSML major newer
	// than the runtime supports. Recoverable : false.
	CodeBundleIncompatible ErrorCode = "BUNDLE_INCOMPATIBLE"

	// CodeVersionGap — runtime detected a missing seq in the server
	// stream. Recoverable : true via reconnect (close, reopen).
	CodeVersionGap ErrorCode = "VERSION_GAP"

	// CodeVersionMismatch — protocol major mismatch (e.g. v=2 client
	// against a v=1 server). Recoverable : false.
	CodeVersionMismatch ErrorCode = "VERSION_MISMATCH"

	// CodeUnknownPath — input frame references a path not in the
	// active scene's operator_inputs. Recoverable : true. Server
	// rejects the entire input frame.
	CodeUnknownPath ErrorCode = "UNKNOWN_PATH"

	// CodeInvalidValue — input value violates the type / constraint
	// declared in the bundle. Recoverable : true. Server rejects
	// the entire input frame.
	CodeInvalidValue ErrorCode = "INVALID_VALUE"

	// CodeRateLimit — connection exceeded a server-side rate limit
	// (commonly inputs/second). Recoverable : true with backoff.
	// The Error frame MAY include retry_after_ms.
	CodeRateLimit ErrorCode = "RATE_LIMIT"

	// CodeTestSessionExpired — test session past TTL. Recoverable :
	// false. Server closes.
	CodeTestSessionExpired ErrorCode = "TEST_SESSION_EXPIRED"

	// CodeInternal — server-side error not covered by a specific code.
	// Recoverable : varies (server sets per case).
	CodeInternal ErrorCode = "INTERNAL"
)

// Recoverable returns the canonical recoverability for a code. The
// CodeInternal case returns the supplied default since recoverability
// varies per scenario for that code.
func (c ErrorCode) Recoverable(internalDefault bool) bool {
	switch c {
	case CodeWriteForbidden,
		CodeBundleFetchFailed,
		CodeVersionGap,
		CodeUnknownPath,
		CodeInvalidValue,
		CodeRateLimit:
		return true
	case CodeAuthDenied,
		CodeSceneNotFound,
		CodeBundleIncompatible,
		CodeVersionMismatch,
		CodeTestSessionExpired:
		return false
	case CodeInternal:
		return internalDefault
	}
	return false
}

// IsKnown reports whether c is a documented LSDP/1 error code.
// Useful for runtime telemetry — surfaces a "spec drift" signal if a
// server emits an unrecognised code.
func (c ErrorCode) IsKnown() bool {
	switch c {
	case CodeAuthDenied,
		CodeWriteForbidden,
		CodeSceneNotFound,
		CodeBundleFetchFailed,
		CodeBundleIncompatible,
		CodeVersionGap,
		CodeVersionMismatch,
		CodeUnknownPath,
		CodeInvalidValue,
		CodeRateLimit,
		CodeTestSessionExpired,
		CodeInternal:
		return true
	}
	return false
}

// LumencastError is the runtime-friendly Go counterpart to the wire
// Error frame. Server emit code is via Error{} ; clients (and the
// conformance harness) consume LumencastError.
type LumencastError struct {
	Code        ErrorCode `json:"code"`
	Message     string    `json:"message"`
	Recoverable bool      `json:"recoverable"`

	// Path is the offending leaf path. Set on the path-scoped codes
	// (WRITE_FORBIDDEN, UNKNOWN_PATH, INVALID_VALUE), empty otherwise
	// — LSDP/1 §3.4.1.
	Path string `json:"path,omitempty"`
}

// Error implements the standard library error interface.
func (e *LumencastError) Error() string {
	return string(e.Code) + ": " + e.Message
}

// FromFrame converts an Error wire frame to a LumencastError.
func FromFrame(f *Error) *LumencastError {
	return &LumencastError{
		Code:        ErrorCode(f.Code),
		Message:     f.Message,
		Recoverable: f.Recoverable,
		Path:        f.Path,
	}
}
