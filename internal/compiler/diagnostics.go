// Package compiler turns a push envelope (Canvas layout + Blue
// blueprint id + component pushed-version refs) into the two
// downstream artifacts every part of Orion consumes:
//
//   - Graph artifact: the topologically-sorted DAG the runtime walks
//     for dirty propagation (ADR 004 § 4).
//   - Render bundle: the Solar-facing tree (primitives only, user
//     components inlined) plus operator_inputs and external_adapters
//     metadata (ADR 003 § 3).
//
// scene_version is sha256(canonical(graph) || canonical(bundle)) so
// two pushes of identical inputs land on the same hash and dedupe in
// scene_pushed_versions on the unique key.
package compiler

import (
	"errors"
	"fmt"
)

// DiagnosticCode is the stable error/warning identifier the compiler
// emits. Codes survive across releases — operators triage on them.
type DiagnosticCode string

// Error codes (ADR 003 § 6 / ADR 004 § 12 + chantier criteria 17, 18).
const (
	ErrCyclicComponent      DiagnosticCode = "CYCLIC_COMPONENT"
	ErrUnknownComponent     DiagnosticCode = "UNKNOWN_COMPONENT"
	ErrUnknownComputeNode   DiagnosticCode = "UNKNOWN_COMPUTE_NODE"
	ErrUnknownPath          DiagnosticCode = "UNKNOWN_PATH"
	ErrInvalidBinding       DiagnosticCode = "INVALID_BINDING"
	ErrInvalidOperatorInput DiagnosticCode = "INVALID_OPERATOR_INPUT"
	ErrInvalidAdapter       DiagnosticCode = "INVALID_ADAPTER"
	ErrFetchUpstream        DiagnosticCode = "FETCH_UPSTREAM"
	ErrTypeMismatch         DiagnosticCode = "TYPE_MISMATCH"
	ErrTopologySort         DiagnosticCode = "TOPOLOGY_SORT"

	// ErrUnknownBlueprintKey (ADR 001 §3.3): a component binding's leading
	// dotted segment names a blueprint key that no BlueprintRef declares.
	// Fails the push closed instead of silently reading nothing.
	ErrUnknownBlueprintKey DiagnosticCode = "UNKNOWN_BLUEPRINT_KEY"

	// ErrDuplicateBlueprintKey (ADR 001 §3.1/§5 R1): two BlueprintRefs share
	// a Key within one envelope — the scene-local handle must be unique or
	// the leaf-path namespacing collides.
	ErrDuplicateBlueprintKey DiagnosticCode = "DUPLICATE_BLUEPRINT_KEY"

	// Platform-event leaf expansion (ADR 003 §3.3.3, issue #84). These are
	// STRUCTURAL validations of a platform node's authored config — never a
	// capability rejection of the node type itself (Orion serves all of
	// Blue; a malformed channel is an authoring error the push surfaces).
	//
	// ErrPlatformChannelMissing: a `quasar.<platform>.<event>@N` node has
	// no usable `config.channel` (absent or empty), so the leaf
	// `__inputs.platform.<platform>.<channel>.last_<event>` cannot expand.
	ErrPlatformChannelMissing DiagnosticCode = "PLATFORM_CHANNEL_MISSING"
	// ErrPlatformChannelInvalid: `config.channel` is present but, AFTER
	// casefolding (strings.ToLower), does not match `^[a-z0-9_]+$` — the
	// fail-closed half of the R4 path-injection defence (Quasar E1 is the
	// producer half). Invalid channels are rejected, never rewritten.
	ErrPlatformChannelInvalid DiagnosticCode = "PLATFORM_CHANNEL_INVALID"

	// ErrSourceNotDeclared (ADR 012 §1.4): a `core.source.read@1` node's
	// authored `source_id` config names no declared source — no
	// ExternalAdapter whose `Key` matches it. source.read is introspection
	// resolved at COMPILE (Option B), so an undeclared source is a
	// push-time STRUCTURAL reject (POST /push), never a runtime error port
	// (a compute has no error pin). Mirrors DATASOURCE_NOT_DECLARED for
	// db.query — fail at push, never on air.
	ErrSourceNotDeclared DiagnosticCode = "SOURCE_NOT_DECLARED"

	// Blueprint-reference compile-time expansion (ADR 014).
	//
	// ErrBlueprintRefUnresolved: a `reference: {blueprint_id, version}` node
	// could not be resolved to a published Blue graph — the blueprint or
	// version does not exist, or the version is still a draft. Maps Blue's
	// typed BLUEPRINT_NOT_FOUND (404) / BLUEPRINT_VERSION_NOT_FOUND (404) /
	// BLUEPRINT_VERSION_NOT_PUBLISHED (422) onto one push-time reject. The
	// push fails closed (latest_pushed_version unchanged), exactly like a
	// malformed envelope — never a silent current_version substitution
	// (ADR 014 §3.4 / §5 version-drift mitigation).
	ErrBlueprintRefUnresolved DiagnosticCode = "BLUEPRINT_REF_UNRESOLVED"

	// ErrBlueprintRefExpansionLimit (ADR 014 §5 graph-explosion mitigation):
	// recursive expansion exceeded the depth/count bound. A deep or wide
	// reference tree blowing up the expanded graph is rejected at push, not
	// at runtime (the cost is paid once, at compile). This is the bound; the
	// full cyclic-reference detector (CYCLIC_BLUEPRINT_REFERENCE) is issue
	// #179 — but a trivial cycle on the resolution stack also trips this
	// bound, so the compiler never loops forever even before #179 lands.
	ErrBlueprintRefExpansionLimit DiagnosticCode = "BLUEPRINT_REF_EXPANSION_LIMIT"
)

// Diagnostic is a single error or warning produced during compilation.
type Diagnostic struct {
	Code     DiagnosticCode `json:"code"`
	Severity string         `json:"severity"` // "error" | "warning"
	Message  string         `json:"message"`
	Path     string         `json:"path,omitempty"` // pointer into the offending artifact
}

// Diagnostics groups errors and warnings produced during a single
// compilation. The compiler always returns this struct (never panics
// on user-supplied data); callers route on Errors().
type Diagnostics struct {
	Items []Diagnostic
}

// Errors returns only the entries with severity = "error".
func (d *Diagnostics) Errors() []Diagnostic {
	var out []Diagnostic
	for _, it := range d.Items {
		if it.Severity == "error" {
			out = append(out, it)
		}
	}
	return out
}

// Warnings returns only the entries with severity = "warning".
func (d *Diagnostics) Warnings() []Diagnostic {
	var out []Diagnostic
	for _, it := range d.Items {
		if it.Severity == "warning" {
			out = append(out, it)
		}
	}
	return out
}

// HasErrors is the binary gate the push handler checks.
func (d *Diagnostics) HasErrors() bool {
	for _, it := range d.Items {
		if it.Severity == "error" {
			return true
		}
	}
	return false
}

// AddError appends an error-severity diagnostic.
func (d *Diagnostics) AddError(code DiagnosticCode, format string, args ...any) {
	d.Items = append(d.Items, Diagnostic{
		Code:     code,
		Severity: "error",
		Message:  fmt.Sprintf(format, args...),
	})
}

// AddErrorAt is like AddError but attaches a source path.
func (d *Diagnostics) AddErrorAt(code DiagnosticCode, path, format string, args ...any) {
	d.Items = append(d.Items, Diagnostic{
		Code:     code,
		Severity: "error",
		Message:  fmt.Sprintf(format, args...),
		Path:     path,
	})
}

// AddWarning appends a warning-severity diagnostic.
func (d *Diagnostics) AddWarning(code DiagnosticCode, format string, args ...any) {
	d.Items = append(d.Items, Diagnostic{
		Code:     code,
		Severity: "warning",
		Message:  fmt.Sprintf(format, args...),
	})
}

// Err converts a diagnostics bag with errors into a Go error so
// the push handler can use errors.Is checks against named codes.
type CompileError struct {
	Diagnostics Diagnostics
}

func (e *CompileError) Error() string {
	if len(e.Diagnostics.Items) == 0 {
		return "compiler: error"
	}
	return fmt.Sprintf("compiler: %d diagnostic(s), first: [%s] %s",
		len(e.Diagnostics.Items),
		e.Diagnostics.Items[0].Code,
		e.Diagnostics.Items[0].Message,
	)
}

// HasCode reports whether any diagnostic in the error matches the
// given code. Lets callers route on `errors.As(...).HasCode(CYCLIC)`.
func (e *CompileError) HasCode(code DiagnosticCode) bool {
	for _, it := range e.Diagnostics.Items {
		if it.Code == code {
			return true
		}
	}
	return false
}

// Is implements errors.Is so callers can check against sentinel
// "any compile error" matchers if desired.
func (e *CompileError) Is(target error) bool {
	_, ok := target.(*CompileError)
	return ok
}

// Sentinel target used by callers that just want "did the compiler
// reject the push".
var ErrCompileFailed = errors.New("compiler: failed")

// Unwrap lets errors.Is(err, ErrCompileFailed) match.
func (e *CompileError) Unwrap() error { return ErrCompileFailed }
