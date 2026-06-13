package runtime

// Tests for the value-equality fix on core.compare.equal@1 /
// core.compare.not-equal@1. The primitives were numeric-only — operands
// went through readNum, so string equality (the command/text matcher
// `lower(payload.text) == "lck"`) errored with `input "a" not a number`
// and never matched. equal/not-equal now compare numerically when both
// operands are numbers (campaign behaviour preserved) and raw/value
// otherwise. The four ORDER comparators stay strictly numeric — pinned
// here so a future refactor can't silently widen them.
//
// Reference semantics: Blue/src/blue/services/executor.py — equal is
// `i.get("a") == i.get("b")`, order comparators coerce via `_num`.
//
// Uses runPure / in from compute_pure_test.go (same package).

import "testing"

func TestCompare_EqualValueAndNumeric(t *testing.T) {
	cases := []struct {
		name string
		id   string
		a, b string
		want string
	}{
		// String equality — the live bug (was always an error → never matched).
		{"eq string match", "core.compare.equal@1", `"lck"`, `"lck"`, `true`},
		{"eq string mismatch", "core.compare.equal@1", `"lck"`, `"lec"`, `false`},
		{"ne string mismatch", "core.compare.not-equal@1", `"lck"`, `"lec"`, `true`},
		{"ne string match", "core.compare.not-equal@1", `"lck"`, `"lck"`, `false`},

		// Numeric non-regression — the campaign-validated branch.
		{"eq num equal", "core.compare.equal@1", `5`, `5`, `true`},
		{"eq num differ", "core.compare.equal@1", `5`, `6`, `false`},
		{"eq int float", "core.compare.equal@1", `5.0`, `5`, `true`},
		{"ne num differ", "core.compare.not-equal@1", `5`, `6`, `true`},
		{"ne int float", "core.compare.not-equal@1", `5.0`, `5`, `false`},

		// Mixed type — number vs string is unequal (Python: 5 == "5" is False).
		{"eq num vs string", "core.compare.equal@1", `5`, `"5"`, `false`},
		{"ne num vs string", "core.compare.not-equal@1", `5`, `"5"`, `true`},

		// Bool / null value equality.
		{"eq bool", "core.compare.equal@1", `true`, `true`, `true`},
		{"eq bool differ", "core.compare.equal@1", `true`, `false`, `false`},
		{"eq null", "core.compare.equal@1", `null`, `null`, `true`},

		// Whitespace-insensitive structural equality (inputs aren't
		// guaranteed byte-canonical the way state writes are).
		{"eq spaced bool", "core.compare.equal@1", ` true `, `true`, `true`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runPure(t, c.id, in("a", c.a, "b", c.b), nil)
			if got != c.want {
				t.Fatalf("%s(%s, %s) = %s, want %s", c.id, c.a, c.b, got, c.want)
			}
		})
	}
}

// TestCompare_EqualReadsXYPorts proves equal/not-equal still read the
// x/y port chain (numeric comparators read x first), so existing wiring
// and the conformance matrix (feeds x=2,y=3) are unaffected.
func TestCompare_EqualReadsXYPorts(t *testing.T) {
	got := runPure(t, "core.compare.equal@1", in("x", `"lck"`, "y", `"lck"`), nil)
	if got != `true` {
		t.Fatalf("equal via x/y ports = %s, want true", got)
	}
	got = runPure(t, "core.compare.equal@1", in("x", `2`, "y", `3`), nil)
	if got != `false` {
		t.Fatalf("equal(x=2,y=3) = %s, want false", got)
	}
}

// TestCompare_OrderComparatorsStayNumeric pins that the ORDER
// comparators are untouched: numeric ordering works, and non-numeric
// operands route through readNum (default 0) rather than gaining a
// value-comparison path. This is the guard against widening order ops.
func TestCompare_OrderComparatorsStayNumeric(t *testing.T) {
	cases := []struct {
		id   string
		a, b string
		want string
	}{
		{"core.compare.less-than@1", `2`, `3`, `true`},
		{"core.compare.less-than@1", `3`, `2`, `false`},
		{"core.compare.less-equal@1", `3`, `3`, `true`},
		{"core.compare.greater-than@1", `3`, `2`, `true`},
		{"core.compare.greater-equal@1", `2`, `3`, `false`},
	}
	for _, c := range cases {
		got := runPure(t, c.id, in("a", c.a, "b", c.b), nil)
		if got != c.want {
			t.Fatalf("%s(%s,%s) = %s, want %s", c.id, c.a, c.b, got, c.want)
		}
	}

	// A non-numeric operand on an ORDER comparator must still error
	// (numeric-only contract) — NOT silently become a value comparison.
	fn, err := NewComputeRegistry().Get("core.compare.less-than@1")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if _, err := fn(in("a", `"lck"`, "b", `"lec"`), nil); err == nil {
		t.Fatal("less-than on strings should error (numeric-only), got nil")
	}
}
