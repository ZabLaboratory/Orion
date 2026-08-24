// Package buildinfo contains provenance injected when Orion is packaged.
package buildinfo

// Unknown is deliberate: a locally compiled Orion must fail Prism's
// embedded-runtime verification instead of pretending to be packaged.
var (
	BlueRuntimeSourceDigest   = "unknown"
	BlueRuntimeSourceRevision = "unknown"
)
