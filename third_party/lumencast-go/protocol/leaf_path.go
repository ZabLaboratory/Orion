package protocol

import (
	"fmt"
	"strings"
	"unicode"
)

// LeafPath is a dotted reference to a single leaf in the subscription
// state map. Examples : "show.title", "players.0.name", "__inputs.locale".
//
// Syntax (from LSDP/1 § 10 + LSML 1.0 § 5) :
//
//   - Segments are alphanumeric + underscore. The first character of a
//     segment MUST be a letter, an underscore, or a digit (numeric
//     indices). Hyphens, spaces and other punctuation are forbidden.
//   - Reserved namespaces start with double underscore : __inputs,
//     __system, __test, __schema. User-defined paths MUST NOT start
//     with double underscore.
//   - A path MAY contain scope substitution placeholders in the
//     {name} form, but ONLY inside an LSML repeat template. The
//     wire-level path that travels in a Patch MUST be fully
//     substituted (no braces).
type LeafPath string

// Validate checks the path is syntactically well-formed for the
// wire layer (no scope placeholders allowed). Use ValidateTemplate
// for the LSML pre-substitution form.
func (p LeafPath) Validate() error {
	return validatePath(string(p), false)
}

// ValidateTemplate is the lenient form used to validate a path inside
// an LSML repeat template. {scope} placeholders are accepted.
func (p LeafPath) ValidateTemplate() error {
	return validatePath(string(p), true)
}

// IsReserved reports whether the path begins with a reserved namespace
// segment ("__inputs", "__system", "__test", "__schema").
func (p LeafPath) IsReserved() bool {
	for _, ns := range reservedNamespaces {
		if string(p) == ns || strings.HasPrefix(string(p), ns+".") {
			return true
		}
	}
	return false
}

// Namespace returns the leading segment (e.g. "__inputs") or the
// empty string if the path is empty.
func (p LeafPath) Namespace() string {
	if i := strings.IndexByte(string(p), '.'); i >= 0 {
		return string(p)[:i]
	}
	return string(p)
}

// HasPrefix reports whether the leaf path falls under a parent prefix.
// Prefix matching is segment-aware : "players" matches "players.0.score"
// but not "playerstats.0".
func (p LeafPath) HasPrefix(prefix string) bool {
	if string(p) == prefix {
		return true
	}
	return strings.HasPrefix(string(p), prefix+".")
}

// Substitute replaces all {name} placeholders in the path using the
// supplied scope map. Returns an error if the template contains a
// placeholder that is not in the map, or if the resulting path is
// not a valid wire-level LeafPath.
//
// Example :
//
//	LeafPath("{player}.score").Substitute(map[string]string{"player": "players.0"})
//	// → "players.0.score"
func (p LeafPath) Substitute(scope map[string]string) (LeafPath, error) {
	src := string(p)
	if !strings.ContainsRune(src, '{') {
		// Fast path : no placeholders.
		out := LeafPath(src)
		if err := out.Validate(); err != nil {
			return "", err
		}
		return out, nil
	}
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		if src[i] != '{' {
			b.WriteByte(src[i])
			i++
			continue
		}
		end := strings.IndexByte(src[i+1:], '}')
		if end < 0 {
			return "", fmt.Errorf("leaf path: unterminated scope placeholder at %d", i)
		}
		name := src[i+1 : i+1+end]
		v, ok := scope[name]
		if !ok {
			return "", fmt.Errorf("leaf path: unknown scope %q", name)
		}
		b.WriteString(v)
		i += end + 2
	}
	out := LeafPath(b.String())
	if err := out.Validate(); err != nil {
		return "", err
	}
	return out, nil
}

var reservedNamespaces = []string{"__inputs", "__system", "__test", "__schema"}

// validatePath is the shared implementation. allowTemplate=true keeps
// {name} placeholders intact for the LSML editor-side validation.
func validatePath(s string, allowTemplate bool) error {
	if s == "" {
		return fmt.Errorf("leaf path: empty")
	}
	segments := strings.Split(s, ".")
	for i, seg := range segments {
		if seg == "" {
			return fmt.Errorf("leaf path: empty segment at %d", i)
		}
		if allowTemplate && strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			inner := seg[1 : len(seg)-1]
			if inner == "" {
				return fmt.Errorf("leaf path: empty scope at %d", i)
			}
			if !isIdentifier(inner) {
				return fmt.Errorf("leaf path: invalid scope %q", inner)
			}
			continue
		}
		if !validSegment(seg) {
			return fmt.Errorf("leaf path: invalid segment %q at %d", seg, i)
		}
	}
	return nil
}

// validSegment accepts letters, digits, underscores. The first char
// may be a digit (numeric indices like "players.0").
func validSegment(seg string) bool {
	for i, r := range seg {
		switch {
		case unicode.IsLetter(r), r == '_':
			// always allowed
		case unicode.IsDigit(r):
			if i == 0 {
				continue
			}
		default:
			return false
		}
	}
	return true
}

// isIdentifier is stricter : letters, digits, underscores, MUST start
// with a letter or underscore. Used for scope names.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case unicode.IsLetter(r), r == '_':
		case unicode.IsDigit(r) && i > 0:
		default:
			return false
		}
	}
	return true
}
