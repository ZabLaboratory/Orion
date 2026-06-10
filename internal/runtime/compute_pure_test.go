package runtime

// Tests for the issue #81 pure data-node tranche (ADR 003 §3.4 phase
// 0). Reference semantics: Blue/src/blue/services/executor.py +
// stdlib_seeder.py. Every node gets named-port unit coverage including
// the null/missing-input contract; the IEEE-754 signed-zero cases are
// pinned explicitly (the -0 hunt — abs/min/max/clamp/round/floor/ceil/
// to-integer/to-float/truthiness); non-commutative nodes (clamp, lerp)
// additionally get shuffled-edge-order scene tests through the REAL
// compiler, and get-field proves the config carriage end-to-end.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// runPure resolves id in the real registry and executes it over named
// inputs + config, returning the raw JSON result as a string.
func runPure(t *testing.T, id string, inputs, config map[string]json.RawMessage) string {
	t.Helper()
	fn, err := NewComputeRegistry().Get(id)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	out, err := fn(inputs, config)
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", id, err)
	}
	return string(out)
}

func in(pairs ...string) map[string]json.RawMessage {
	if len(pairs)%2 != 0 {
		panic("in: odd pairs")
	}
	m := make(map[string]json.RawMessage, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i]] = json.RawMessage(pairs[i+1])
	}
	return m
}

// ---------------------------------------------------------------------------
// Logic — Python truthiness over any JSON value
// ---------------------------------------------------------------------------

func TestPure_Logic(t *testing.T) {
	cases := []struct {
		id     string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"core.logic.and@1", in("a", `true`, "b", `true`), `true`},
		{"core.logic.and@1", in("a", `true`, "b", `false`), `false`},
		{"core.logic.and@1", in("a", `true`), `false`}, // missing b → false
		{"core.logic.and@1", in(), `false`},            // both missing
		{"core.logic.and@1", in("a", `true`, "b", `null`), `false`},
		{"core.logic.and@1", in("a", `"x"`, "b", `1`), `true`}, // truthiness coercion
		{"core.logic.and@1", in("a", `"x"`, "b", `0`), `false`},
		{"core.logic.and@1", in("a", `[1]`, "b", `{"k":1}`), `true`},
		{"core.logic.and@1", in("a", `[]`, "b", `true`), `false`},   // empty list falsy
		{"core.logic.and@1", in("a", `{}`, "b", `true`), `false`},   // empty obj falsy
		{"core.logic.and@1", in("a", `-0.0`, "b", `true`), `false`}, // -0 is falsy (== 0)
		{"core.logic.or@1", in("a", `false`, "b", `true`), `true`},
		{"core.logic.or@1", in("a", `false`, "b", `false`), `false`},
		{"core.logic.or@1", in(), `false`},
		{"core.logic.or@1", in("a", `""`, "b", `"0"`), `true`}, // "0" non-empty → truthy
		{"core.logic.xor@1", in("a", `true`, "b", `true`), `false`},
		{"core.logic.xor@1", in("a", `true`, "b", `false`), `true`},
		{"core.logic.xor@1", in("a", `false`, "b", `true`), `true`},
		{"core.logic.xor@1", in(), `false`},
		{"core.logic.xor@1", in("a", `1`, "b", `null`), `true`},
	}
	for _, c := range cases {
		if got := runPure(t, c.id, c.inputs, nil); got != c.want {
			t.Errorf("%s(%v) = %s, want %s", c.id, c.inputs, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Extended math
// ---------------------------------------------------------------------------

func TestPure_MathAbs(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("value", `5`), `5`},
		{in("value", `-5.5`), `5.5`},
		{in("value", `0`), `0`},
		{in("value", `-0.0`), `0`},    // -0 hunt: abs(-0) must be +0, never "-0"
		{in("value", `"3.5"`), `3.5`}, // _num string coercion
		{in("value", `null`), `0`},
		{in(), `0`}, // missing → default 0
	}
	for _, c := range cases {
		if got := runPure(t, "core.math.abs@1", c.inputs, nil); got != c.want {
			t.Errorf("abs(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

// TestPure_MathMinMax_SignedZero pins Python's first-argument-wins tie
// semantics, observable only with signed zeros: min(-0.0, 0.0) keeps
// the FIRST argument (-0.0) while min(0.0, -0.0) keeps 0.0. Go's
// math.Min/math.Max would return -0/+0 regardless of order — using
// them here is exactly the bug class caught three times on 2026-06-09.
func TestPure_MathMinMax_SignedZero(t *testing.T) {
	cases := []struct {
		id     string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"core.math.min@1", in("a", `-0.0`, "b", `0`), `-0`},
		{"core.math.min@1", in("a", `0`, "b", `-0.0`), `0`},
		{"core.math.max@1", in("a", `-0.0`, "b", `0`), `-0`},
		{"core.math.max@1", in("a", `0`, "b", `-0.0`), `0`},
	}
	for _, c := range cases {
		if got := runPure(t, c.id, c.inputs, nil); got != c.want {
			t.Errorf("%s(%v) = %s, want %s (Python first-wins tie)", c.id, c.inputs, got, c.want)
		}
	}
}

func TestPure_MathMinMax(t *testing.T) {
	cases := []struct {
		id     string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"core.math.min@1", in("a", `3`, "b", `7`), `3`},
		{"core.math.min@1", in("a", `7`, "b", `3`), `3`},
		{"core.math.min@1", in("a", `-2`, "b", `1`), `-2`},
		{"core.math.min@1", in("a", `5`), `0`}, // missing b → 0
		{"core.math.min@1", in(), `0`},
		{"core.math.max@1", in("a", `3`, "b", `7`), `7`},
		{"core.math.max@1", in("a", `-2`, "b", `-9`), `-2`},
		{"core.math.max@1", in("b", `-9`), `0`},              // missing a → 0 wins
		{"core.math.max@1", in("a", `null`, "b", `-1`), `0`}, // null coerces to default
	}
	for _, c := range cases {
		if got := runPure(t, c.id, c.inputs, nil); got != c.want {
			t.Errorf("%s(%v) = %s, want %s", c.id, c.inputs, got, c.want)
		}
	}
}

func TestPure_MathClamp(t *testing.T) {
	cases := []struct {
		name   string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"inside", in("value", `0.5`, "min", `0`, "max", `1`), `0.5`},
		{"below", in("value", `-3`, "min", `0`, "max", `1`), `0`},
		{"above", in("value", `42`, "min", `0`, "max", `10`), `10`},
		{"default bounds", in("value", `7`), `1`},      // min→0, max→1 (signature defaults)
		{"default bounds low", in("value", `-7`), `0`}, // clamps to default min 0
		{"all missing", in(), `0`},                     // value 0 in [0,1]
		// Pathological lo > hi: the reference is max(lo, min(hi, v)) —
		// the lower bound wins. clamp(5, min=3, max=1) = max(3, 1) = 3.
		{"lo>hi", in("value", `5`, "min", `3`, "max", `1`), `3`},
		// -0 hunt: clamp(-0, 0, 1) → min(1,-0)=-0, max(0,-0)=0 → "0".
		{"neg zero vs zero lo", in("value", `-0.0`, "min", `0`, "max", `1`), `0`},
		// clamp(-0, -1, 1) → min(1,-0)=-0, max(-1,-0)=-0 → "-0" survives.
		{"neg zero inside", in("value", `-0.0`, "min", `-1`, "max", `1`), `-0`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.math.clamp@1", c.inputs, nil); got != c.want {
			t.Errorf("clamp %s (%v) = %s, want %s", c.name, c.inputs, got, c.want)
		}
	}
}

func TestPure_MathLerp(t *testing.T) {
	cases := []struct {
		name   string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"midpoint", in("a", `0`, "b", `10`, "alpha", `0.5`), `5`},
		{"start", in("a", `2`, "b", `10`, "alpha", `0`), `2`},
		{"end", in("a", `2`, "b", `10`, "alpha", `1`), `10`},
		{"extrapolates", in("a", `0`, "b", `10`, "alpha", `2`), `20`}, // NOT clamped
		{"negative alpha", in("a", `0`, "b", `10`, "alpha", `-1`), `-10`},
		{"defaults", in(), `0.5`},                                     // a=0, b=1, alpha=0.5
		{"default b alpha", in("a", `1`), `1`},                        // 1 + (1-1)*0.5
		{"null alpha", in("a", `0`, "b", `10`, "alpha", `null`), `5`}, // null → default
	}
	for _, c := range cases {
		if got := runPure(t, "core.math.lerp@1", c.inputs, nil); got != c.want {
			t.Errorf("lerp %s (%v) = %s, want %s", c.name, c.inputs, got, c.want)
		}
	}
}

// TestPure_MathRound pins banker's rounding (the seeder documents
// "banker's rounding via Python's built-in") and the -0 trap:
// RoundToEven(-0.5) and RoundToEven(-0.2) are -0.0 in Go, but the
// output is INTEGER-typed (a Python int has no signed zero) so the
// wire value must be "0".
func TestPure_MathRound(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("value", `2.4`), `2`},
		{in("value", `2.6`), `3`},
		{in("value", `0.5`), `0`}, // half-to-even, not half-up
		{in("value", `1.5`), `2`},
		{in("value", `2.5`), `2`}, // banker's: 2.5 → 2, not 3
		{in("value", `-1.5`), `-2`},
		{in("value", `-2.5`), `-2`},
		{in("value", `-0.5`), `0`}, // -0 hunt: RoundToEven gives -0 → "0"
		{in("value", `-0.2`), `0`}, // -0 hunt
		{in("value", `-0.0`), `0`}, // -0 hunt
		{in(), `0`},
		{in("value", `null`), `0`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.math.round@1", c.inputs, nil); got != c.want {
			t.Errorf("round(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

func TestPure_MathFloorCeil(t *testing.T) {
	cases := []struct {
		id     string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"core.math.floor@1", in("value", `1.7`), `1`},
		{"core.math.floor@1", in("value", `-1.2`), `-2`},
		{"core.math.floor@1", in("value", `-0.3`), `-1`},
		{"core.math.floor@1", in("value", `2`), `2`},
		{"core.math.floor@1", in("value", `-0.0`), `0`}, // -0 hunt: Floor(-0) is -0 in Go
		{"core.math.floor@1", in(), `0`},
		{"core.math.ceil@1", in("value", `1.2`), `2`},
		{"core.math.ceil@1", in("value", `-1.7`), `-1`},
		{"core.math.ceil@1", in("value", `-0.3`), `0`}, // -0 hunt: Go Ceil(-0.3) = -0, Python int 0
		{"core.math.ceil@1", in("value", `-0.0`), `0`}, // -0 hunt
		{"core.math.ceil@1", in("value", `2`), `2`},
		{"core.math.ceil@1", in(), `0`},
	}
	for _, c := range cases {
		if got := runPure(t, c.id, c.inputs, nil); got != c.want {
			t.Errorf("%s(%v) = %s, want %s", c.id, c.inputs, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// String
// ---------------------------------------------------------------------------

func TestPure_StringConcat(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("a", `"foo"`, "b", `"bar"`), `"foobar"`},
		{in("a", `"score: "`, "b", `3.5`), `"score: 3.5"`}, // _str renders numbers
		{in("a", `"x"`, "b", `null`), `"x"`},               // null → ""
		{in("a", `"x"`), `"x"`},                            // missing → ""
		{in(), `""`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.string.concat@1", c.inputs, nil); got != c.want {
			t.Errorf("concat(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

func TestPure_StringFormat(t *testing.T) {
	cases := []struct {
		name   string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"basic", in("template", `"{name} wins"`, "args", `{"name":"Zab"}`), `"Zab wins"`},
		{"two keys", in("template", `"{a}-{b}"`, "args", `{"a":"x","b":"y"}`), `"x-y"`},
		{"number arg", in("template", `"score {n}"`, "args", `{"n":14}`), `"score 14"`},
		{"missing key → raw template", in("template", `"{ghost}"`, "args", `{"name":"Zab"}`), `"{ghost}"`},
		{"args not object → raw", in("template", `"{name}"`, "args", `[1,2]`), `"{name}"`},
		{"args null → raw", in("template", `"{name}"`, "args", `null`), `"{name}"`},
		{"args missing → raw", in("template", `"{name}"`), `"{name}"`},
		{"escaped braces", in("template", `"{{literal}} {name}"`, "args", `{"name":"v"}`), `"{literal} v"`},
		{"positional → raw", in("template", `"{} x"`, "args", `{"name":"v"}`), `"{} x"`},
		{"format spec → raw", in("template", `"{n:.2f}"`, "args", `{"n":1}`), `"{n:.2f}"`},
		{"unmatched brace → raw", in("template", `"{oops"`, "args", `{"oops":1}`), `"{oops"`},
		{"lone closing → raw", in("template", `"a} b"`, "args", `{}`), `"a} b"`},
		{"no placeholders", in("template", `"plain"`, "args", `{}`), `"plain"`},
		{"template missing", in("args", `{"a":1}`), `""`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.string.format@1", c.inputs, nil); got != c.want {
			t.Errorf("format %s = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestPure_StringLength(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("value", `"hello"`), `5`},
		{in("value", `"héllo"`), `5`}, // code points, not bytes
		{in("value", `""`), `0`},
		{in("value", `null`), `0`},
		{in(), `0`},
		{in("value", `3.5`), `3`}, // _str("3.5") → len 3
	}
	for _, c := range cases {
		if got := runPure(t, "core.string.length@1", c.inputs, nil); got != c.want {
			t.Errorf("length(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

func TestPure_StringSplit(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("value", `"a,b,c"`, "separator", `","`), `["a","b","c"]`},
		{in("value", `"a|b"`, "separator", `"|"`), `["a","b"]`},
		{in("value", `"a,b"`), `["a","b"]`},                    // missing separator → ","
		{in("value", `"a,b"`, "separator", `""`), `["a","b"]`}, // empty separator → "," (reference `or ","`)
		{in("value", `""`), `[""]`},                            // Python "".split(",") == [""]
		{in(), `[""]`},
		{in("value", `"no-sep-here"`, "separator", `";"`), `["no-sep-here"]`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.string.split@1", c.inputs, nil); got != c.want {
			t.Errorf("split(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

func TestPure_StringUpperLower(t *testing.T) {
	cases := []struct {
		id     string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"core.string.upper@1", in("value", `"abc"`), `"ABC"`},
		{"core.string.upper@1", in("value", `"héllo"`), `"HÉLLO"`}, // Unicode-aware
		{"core.string.upper@1", in("value", `null`), `""`},
		{"core.string.upper@1", in(), `""`},
		{"core.string.lower@1", in("value", `"AbC"`), `"abc"`},
		{"core.string.lower@1", in("value", `"ÉCRAN"`), `"écran"`},
		{"core.string.lower@1", in(), `""`},
	}
	for _, c := range cases {
		if got := runPure(t, c.id, c.inputs, nil); got != c.want {
			t.Errorf("%s(%v) = %s, want %s", c.id, c.inputs, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Cast
// ---------------------------------------------------------------------------

func TestPure_CastToString(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("value", `"already"`), `"already"`},
		{in("value", `3.5`), `"3.5"`},
		{in("value", `true`), `"true"`}, // JSON form (documented divergence from Python "True")
		{in("value", `null`), `""`},
		{in(), `""`},
		{in("value", `{"a":1}`), `"{\"a\":1}"`}, // compact JSON, not Python repr
		{in("value", `[1,2]`), `"[1,2]"`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.cast.to-string@1", c.inputs, nil); got != c.want {
			t.Errorf("to-string(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

func TestPure_CastToInteger(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("value", `42`), `42`},
		{in("value", `3.9`), `3`},   // truncates toward zero (Python int())
		{in("value", `-3.9`), `-3`}, // NOT floor — toward zero
		{in("value", `-0.5`), `0`},  // -0 hunt: Trunc(-0.5) is -0 in Go → "0"
		{in("value", `-0.0`), `0`},  // -0 hunt
		{in("value", `"42"`), `42`},
		{in("value", `" 42 "`), `42`}, // Python int() strips whitespace
		{in("value", `"+7"`), `7`},
		{in("value", `"3.5"`), `0`}, // Python int("3.5") is a ValueError → 0
		{in("value", `"x"`), `0`},
		{in("value", `true`), `1`}, // bool subclasses int in the reference
		{in("value", `false`), `0`},
		{in("value", `null`), `0`}, // str(None) → ValueError → 0
		{in(), `0`},
		{in("value", `[1]`), `0`},
		{in("value", `{"a":1}`), `0`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.cast.to-integer@1", c.inputs, nil); got != c.want {
			t.Errorf("to-integer(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

func TestPure_CastToFloat(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("value", `3.5`), `3.5`},
		{in("value", `7`), `7`},
		{in("value", `"2.5"`), `2.5`},
		{in("value", `" 2.5 "`), `2.5`},
		{in("value", `"-0.0"`), `-0`}, // -0 hunt: Python float("-0.0") keeps the sign — so do we
		{in("value", `"x"`), `0`},
		{in("value", `true`), `1`},
		{in("value", `false`), `0`},
		{in("value", `null`), `0`},
		{in(), `0`},
		{in("value", `[1]`), `0`},
		{in("value", `"inf"`), `null`}, // non-finite is unrepresentable in JSON → null (documented)
	}
	for _, c := range cases {
		if got := runPure(t, "core.cast.to-float@1", c.inputs, nil); got != c.want {
			t.Errorf("to-float(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

func TestPure_CastToBoolean(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("value", `true`), `true`},
		{in("value", `false`), `false`},
		{in("value", `0`), `false`},
		{in("value", `-0.0`), `false`}, // -0 hunt: -0 == 0 → falsy
		{in("value", `0.1`), `true`},
		{in("value", `-1`), `true`},
		{in("value", `""`), `false`},
		{in("value", `"0"`), `true`}, // non-empty string truthy, like Python
		{in("value", `null`), `false`},
		{in(), `false`},
		{in("value", `[]`), `false`},
		{in("value", `[0]`), `true`},
		{in("value", `{}`), `false`},
		{in("value", `{"a":0}`), `true`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.cast.to-boolean@1", c.inputs, nil); got != c.want {
			t.Errorf("to-boolean(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Data
// ---------------------------------------------------------------------------

func cfg(pairs ...string) map[string]json.RawMessage { return in(pairs...) }

// TestPure_DataGetField is the phase 2 payload extractor — quality is
// non-negotiable here (ADR 003 §3.3: quasar.* events are objects this
// node dissects).
func TestPure_DataGetField(t *testing.T) {
	record := `{"user":{"name":"Zab","tags":["a","b","c"]},"scores":{"1":"one"},"n":0}`
	cases := []struct {
		name   string
		inputs map[string]json.RawMessage
		config map[string]json.RawMessage
		want   string
	}{
		{"top-level", in("record", record), cfg("path", `"n"`), `0`},
		{"nested", in("record", record), cfg("path", `"user.name"`), `"Zab"`},
		{"list index", in("record", record), cfg("path", `"user.tags.1"`), `"b"`},
		{"negative index (Python lst[-1])", in("record", record), cfg("path", `"user.tags.-1"`), `"c"`},
		{"numeric key on object", in("record", record), cfg("path", `"scores.1"`), `"one"`},
		{"missing field", in("record", record), cfg("path", `"user.ghost"`), `null`},
		{"missing deep", in("record", record), cfg("path", `"user.ghost.deeper"`), `null`},
		{"index out of range", in("record", record), cfg("path", `"user.tags.9"`), `null`},
		{"bad list index", in("record", record), cfg("path", `"user.tags.x"`), `null`},
		{"walk through scalar", in("record", record), cfg("path", `"n.sub"`), `null`},
		{"empty path → record", in("record", `{"a":1}`), cfg("path", `""`), `{"a":1}`},
		{"missing path config → record", in("record", `{"a":1}`), nil, `{"a":1}`},
		{"record null", in("record", `null`), cfg("path", `"a"`), `null`},
		{"record missing", in(), cfg("path", `"a"`), `null`},
		{"record scalar", in("record", `42`), cfg("path", `"a"`), `null`},
		{"record is a list", in("record", `[10,20]`), cfg("path", `"1"`), `20`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.data.get-field@1", c.inputs, c.config); got != c.want {
			t.Errorf("get-field %s = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestPure_DataSetField(t *testing.T) {
	cases := []struct {
		name   string
		inputs map[string]json.RawMessage
		config map[string]json.RawMessage
		want   string
	}{
		{"replace", in("record", `{"a":1}`, "value", `2`), cfg("path", `"a"`), `{"a":2}`},
		{"add", in("record", `{"a":1}`, "value", `true`), cfg("path", `"b"`), `{"a":1,"b":true}`},
		{"nested create", in("record", `{}`, "value", `"x"`), cfg("path", `"u.name"`), `{"u":{"name":"x"}}`},
		{"nested preserve siblings", in("record", `{"u":{"keep":1}}`, "value", `2`), cfg("path", `"u.set"`), `{"u":{"keep":1,"set":2}}`},
		{"overwrite non-object intermediate", in("record", `{"u":5}`, "value", `1`), cfg("path", `"u.x"`), `{"u":{"x":1}}`},
		{"record null → fresh object", in("record", `null`, "value", `1`), cfg("path", `"a"`), `{"a":1}`},
		{"record missing → fresh object", in("value", `1`), cfg("path", `"a"`), `{"a":1}`},
		{"record list → fresh object (reference: dict only)", in("record", `[1]`, "value", `1`), cfg("path", `"a"`), `{"a":1}`},
		{"empty path → value", in("record", `{"a":1}`, "value", `"v"`), cfg("path", `""`), `"v"`},
		{"missing value → null set", in("record", `{}`), cfg("path", `"a"`), `{"a":null}`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.data.set-field@1", c.inputs, c.config); got != c.want {
			t.Errorf("set-field %s = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestPure_DataListLength(t *testing.T) {
	cases := []struct {
		inputs map[string]json.RawMessage
		want   string
	}{
		{in("list", `[1,2,3]`), `3`},
		{in("list", `[]`), `0`},
		{in("list", `null`), `0`},
		{in("list", `"abc"`), `0`}, // a string is not a list (reference isinstance guard)
		{in("list", `{"a":1}`), `0`},
		{in(), `0`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.data.list-length@1", c.inputs, nil); got != c.want {
			t.Errorf("list-length(%v) = %s, want %s", c.inputs, got, c.want)
		}
	}
}

func TestPure_DataListAt(t *testing.T) {
	lst := `["a","b","c"]`
	cases := []struct {
		name   string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"basic", in("list", lst, "index", `1`), `"b"`},
		{"zero", in("list", lst, "index", `0`), `"a"`},
		{"missing index → signature default 0", in("list", lst), `"a"`},
		{"out of range", in("list", lst, "index", `9`), `null`},
		{"negative → null (explicit 0<= guard, no Python wrap)", in("list", lst, "index", `-1`), `null`},
		{"float index → null (strict int guard)", in("list", lst, "index", `1.0`), `null`},
		{"exponent index → null (json float)", in("list", lst, "index", `1e0`), `null`},
		{"string index → null", in("list", lst, "index", `"1"`), `null`},
		{"null index → null (wired None is not an int)", in("list", lst, "index", `null`), `null`},
		{"bool index → null (documented divergence from bool-is-int)", in("list", lst, "index", `true`), `null`},
		{"list null", in("list", `null`, "index", `0`), `null`},
		{"list missing", in("index", `0`), `null`},
		{"list not a list", in("list", `"abc"`, "index", `0`), `null`},
		{"element is object", in("list", `[{"k":1}]`, "index", `0`), `{"k":1}`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.data.list-at@1", c.inputs, nil); got != c.want {
			t.Errorf("list-at %s = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestPure_DataListAppend(t *testing.T) {
	cases := []struct {
		name   string
		inputs map[string]json.RawMessage
		want   string
	}{
		{"basic", in("list", `[1,2]`, "element", `3`), `[1,2,3]`},
		{"object element", in("list", `[]`, "element", `{"k":1}`), `[{"k":1}]`},
		{"list null → fresh", in("list", `null`, "element", `1`), `[1]`},
		{"list missing → fresh", in("element", `1`), `[1]`},
		{"list not a list → fresh", in("list", `"x"`, "element", `1`), `[1]`},
		{"element missing → null appended", in("list", `[1]`), `[1,null]`},
		{"both missing → [null]", in(), `[null]`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.data.list-append@1", c.inputs, nil); got != c.want {
			t.Errorf("list-append %s = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestPure_DataAggregate(t *testing.T) {
	cases := []struct {
		name   string
		inputs map[string]json.RawMessage
		config map[string]json.RawMessage
		want   string
	}{
		{"sum", in("items", `[1,2,3]`), cfg("op", `"sum"`), `6`},
		{"avg", in("items", `[1,2,3]`), cfg("op", `"avg"`), `2`},
		{"min", in("items", `[3,1,2]`), cfg("op", `"min"`), `1`},
		{"max", in("items", `[3,9,2]`), cfg("op", `"max"`), `9`},
		{"count", in("items", `[3,9,2]`), cfg("op", `"count"`), `3`},
		{"count counts non-numerics", in("items", `["a",null,{}]`), cfg("op", `"count"`), `3`},
		{"default op is sum", in("items", `[1,2]`), nil, `3`},
		{"empty op is sum", in("items", `[1,2]`), cfg("op", `""`), `3`},
		{"empty list", in("items", `[]`), cfg("op", `"count"`), `0`},
		{"items null", in("items", `null`), cfg("op", `"sum"`), `0`},
		{"items missing", in(), cfg("op", `"sum"`), `0`},
		{"items not a list", in("items", `5`), cfg("op", `"sum"`), `0`},
		{"non-numeric coerce to 0", in("items", `[1,"x",2]`), cfg("op", `"sum"`), `3`},
		{"numeric strings coerce", in("items", `["1","2"]`), cfg("op", `"sum"`), `3`},
		{"unknown op", in("items", `[1,2]`), cfg("op", `"median"`), `0`},
		// -0 hunt: min over [-0.0, 0.0] keeps the first minimal (-0).
		{"min signed zero first-wins", in("items", `[-0.0, 0]`), cfg("op", `"min"`), `-0`},
		{"min signed zero order flip", in("items", `[0, -0.0]`), cfg("op", `"min"`), `0`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.data.aggregate@1", c.inputs, c.config); got != c.want {
			t.Errorf("aggregate %s = %s, want %s", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Named ports through the REAL compiler — shuffled edge order on the
// non-commutative nodes, and config carriage for get-field
// ---------------------------------------------------------------------------

// TestPure_NamedPorts_ClampShuffledEdges authors clamp's three edges in
// scrambled order (max, value, min). Positional zip would deliver
// max→`a` etc. and clamp would misread every port; the carried to_port
// names must wire it correctly: clamp(15, min=0, max=10) = 10.
func TestPure_NamedPorts_ClampShuffledEdges(t *testing.T) {
	runShuffledEdgeScene(t, shuffledSceneSpec{
		compute: "core.math.clamp@1",
		literals: map[string]string{
			"lit.v":   `15`,
			"lit.min": `0`,
			"lit.max": `10`,
		},
		// Deliberately scrambled: max first, value second, min last.
		edges: [][2]string{
			{"lit.max", "max"},
			{"lit.v", "value"},
			{"lit.min", "min"},
		},
		want: `10`,
	})
}

// TestPure_NamedPorts_LerpShuffledEdges — lerp(a=0, b=10, alpha=0.25)
// = 2.5 with edges authored alpha-first. A positional zip would read
// alpha as `a` and break.
func TestPure_NamedPorts_LerpShuffledEdges(t *testing.T) {
	runShuffledEdgeScene(t, shuffledSceneSpec{
		compute: "core.math.lerp@1",
		literals: map[string]string{
			"lit.a":     `0`,
			"lit.b":     `10`,
			"lit.alpha": `0.25`,
		},
		edges: [][2]string{
			{"lit.alpha", "alpha"},
			{"lit.b", "b"},
			{"lit.a", "a"},
		},
		want: `2.5`,
	})
}

type shuffledSceneSpec struct {
	compute  string
	literals map[string]string // literal node id → JSON value
	edges    [][2]string       // {from literal id, to_port on the compute}
	want     string            // expected leaf value at "result.leaf"
}

func runShuffledEdgeScene(t *testing.T, spec shuffledSceneSpec) {
	t.Helper()
	bp := &compiler.BlueprintGraph{ID: "bp-shuffle"}
	manifest := compiler.ComputeManifest{
		"core.literal@1": {IsPure: true, IsBounded: true, Version: "1"},
		"core.output@1":  {IsPure: true, IsBounded: true, Version: "1"},
		spec.compute:     {IsPure: true, IsBounded: true, Version: "1"},
	}
	for id, val := range spec.literals {
		bp.Nodes = append(bp.Nodes, compiler.BlueprintNode{
			ID: id, Compute: "core.literal@1",
			Config: map[string]json.RawMessage{"value": json.RawMessage(val)},
		})
	}
	bp.Nodes = append(bp.Nodes,
		compiler.BlueprintNode{ID: "cmp", Compute: spec.compute},
		compiler.BlueprintNode{ID: "out", Compute: "core.output@1",
			Config: map[string]json.RawMessage{"name": json.RawMessage(`"result.leaf"`)}},
	)
	for _, e := range spec.edges {
		bp.Edges = append(bp.Edges, compiler.BlueprintEdge{
			FromNode: e[0], FromPort: "value", ToNode: "cmp", ToPort: e[1],
		})
	}
	bp.Edges = append(bp.Edges, compiler.BlueprintEdge{
		FromNode: "cmp", FromPort: "result", ToNode: "out", ToPort: "value",
	})

	f := &stubFetcher{
		layout: &compiler.CanvasLayout{
			Version: "v1",
			Root:    compiler.LayoutNode{Kind: "stack", ID: "root"},
		},
		blueprint: bp,
		manifest:  manifest,
	}
	graph, bundle, _, err := compiler.Compile(context.Background(), "scene-shuffle",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-shuffle"}, f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	scene := NewScene("scene-shuffle", graph, bundle, NewComputeRegistry(), quietLogger())
	_, snap := scene.Subscribe(8)
	if got := string(snap.State["result.leaf"]); got != spec.want {
		t.Fatalf("%s shuffled edges: result.leaf = %s, want %s (positional wiring would misdeliver)",
			spec.compute, got, spec.want)
	}
}

// TestPure_NamedPorts_GetFieldConfigCarried proves the FULL phase 2
// extraction path: a blueprint get-field node's `config.path` survives
// compile (GraphNode.Config, issue #81), and at runtime the node pulls
// the right field out of an upstream JSON record and lands it on an
// output leaf.
func TestPure_NamedPorts_GetFieldConfigCarried(t *testing.T) {
	bp := &compiler.BlueprintGraph{
		ID: "bp-getfield",
		Nodes: []compiler.BlueprintNode{
			{ID: "lit.payload", Compute: "core.literal@1",
				Config: map[string]json.RawMessage{"value": json.RawMessage(`{"user":{"name":"Clodo"},"score":14}`)}},
			{ID: "extract", Compute: "core.data.get-field@1",
				Config: map[string]json.RawMessage{"path": json.RawMessage(`"user.name"`)}},
			{ID: "out", Compute: "core.output@1",
				Config: map[string]json.RawMessage{"name": json.RawMessage(`"display.user"`)}},
		},
		Edges: []compiler.BlueprintEdge{
			{FromNode: "lit.payload", FromPort: "value", ToNode: "extract", ToPort: "record"},
			{FromNode: "extract", FromPort: "value", ToNode: "out", ToPort: "value"},
		},
	}
	f := &stubFetcher{
		layout: &compiler.CanvasLayout{
			Version: "v1",
			Root:    compiler.LayoutNode{Kind: "stack", ID: "root"},
		},
		blueprint: bp,
		manifest: compiler.ComputeManifest{
			"core.literal@1":        {IsPure: true, IsBounded: true, Version: "1"},
			"core.output@1":         {IsPure: true, IsBounded: true, Version: "1"},
			"core.data.get-field@1": {IsPure: true, IsBounded: true, Version: "1"},
		},
	}
	graph, bundle, _, err := compiler.Compile(context.Background(), "scene-getfield",
		compiler.PushEnvelope{CanvasVersion: "v1", BlueBlueprintID: "bp-getfield"}, f)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// The artefact must carry the config on the computed node.
	carried := false
	for _, n := range graph.Nodes {
		if n.ID == "extract" && len(n.Config) > 0 {
			carried = true
		}
	}
	if !carried {
		t.Fatal("GraphNode.Config not carried for the get-field node — config-bearing computes cannot run")
	}

	scene := NewScene("scene-getfield", graph, bundle, NewComputeRegistry(), quietLogger())
	_, snap := scene.Subscribe(8)
	if got := string(snap.State["display.user"]); got != `"Clodo"` {
		t.Fatalf(`get-field via compiled artefact: display.user = %s, want "Clodo"`, got)
	}
}
