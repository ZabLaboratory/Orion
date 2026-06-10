package runtime

// Probe tests — issue #81 pure data-node tranche, ADR 003 §3.4 phase 0.
//
// This file complements compute_pure_test.go (Forge's proximity suite).
// It targets the specific gaps identified in the -0 hunt, get-field
// robustness (phase 2 payload extractor), configStr null path, aggregate
// max signed-zero symmetry, non-finite propagation, and a set of
// malformed-input paths that Forge's suite left untouched.
//
// Refs #81

import (
	"encoding/json"
	"testing"
)

// ---------------------------------------------------------------------------
// IEEE-754 signed zero — aggregate max (symmetric gap to the min tests)
// ---------------------------------------------------------------------------

// TestProbe_AggregateMax_SignedZero mirrors the min signed-zero tests:
// pyMax first-argument-wins ties — max([-0.0, 0]) = -0, max([0, -0.0]) = 0.
// Forge's TestPure_DataAggregate only pins the min direction; this pins max.
func TestProbe_AggregateMax_SignedZero(t *testing.T) {
	cases := []struct {
		name   string
		inputs map[string]json.RawMessage
		want   string
	}{
		// pyMax(a, b) returns b only when b > a strictly.
		// -0 == 0 in IEEE-754, so 0 > -0 is false → incumbent (-0) wins.
		{"max signed zero first -0", in("items", `[-0.0, 0]`), `-0`},
		// pyMax(0, -0): -0 > 0 is false → incumbent (0) wins.
		{"max signed zero first +0", in("items", `[0, -0.0]`), `0`},
		// Three-element: max([0, -0.0, 0.0]) — accumulator starts at 0,
		// none of the subsequent values strictly exceeds it → stays 0.
		{"max three zeroes +0 first", in("items", `[0, -0.0, 0]`), `0`},
		// Three-element: max([-0.0, 0, 0]) — accumulator starts at -0,
		// 0 > -0 is false (equal) → stays -0.
		{"max three zeroes -0 first", in("items", `[-0.0, 0, 0]`), `-0`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.data.aggregate@1", c.inputs, cfg("op", `"max"`)); got != c.want {
			t.Errorf("aggregate max signed-zero %s = %s, want %s (pyMax first-wins broken)", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Aggregate: additional coverage gaps
// ---------------------------------------------------------------------------

func TestProbe_AggregateAvgSingle(t *testing.T) {
	// avg([5]) = 5/1 = 5. Regression guard: division by len avoids zero div.
	if got := runPure(t, "core.data.aggregate@1", in("items", `[5]`), cfg("op", `"avg"`)); got != `5` {
		t.Errorf("aggregate avg single = %s, want 5", got)
	}
}

func TestProbe_AggregateMinMaxTieNoZero(t *testing.T) {
	// Tie with identical non-zero values — first-wins must keep the first.
	cases := []struct {
		name string
		op   string
		want string
	}{
		{"min tie positive", "min", `3`},
		{"max tie positive", "max", `3`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.data.aggregate@1", in("items", `[3, 3]`), cfg("op", `"`+c.op+`"`)); got != c.want {
			t.Errorf("aggregate %s tie: %s, want %s", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Non-finite propagation — clamp, lerp, min, max, abs
// ---------------------------------------------------------------------------

// Blue documents non-finite → null (jsonNum). These paths are reachable
// because pyNum parses numeric strings including "inf"/"nan" via
// strconv.ParseFloat. An authored literal "inf" as a string would coerce
// and then produce null on the wire — this must not panic or produce "+Inf".

func TestProbe_NonFinite_Propagation(t *testing.T) {
	cases := []struct {
		id     string
		inputs map[string]json.RawMessage
		want   string
	}{
		// Inf input to abs → still Inf → null
		{"core.math.abs@1", in("value", `"inf"`), `null`},
		// min(Inf, 1) = 1
		{"core.math.min@1", in("a", `"inf"`, "b", `1`), `1`},
		// max(1, Inf) = Inf → null
		{"core.math.max@1", in("a", `1`, "b", `"inf"`), `null`},
		// lerp with NaN alpha → NaN result → null
		{"core.math.lerp@1", in("a", `0`, "b", `10`, "alpha", `"nan"`), `null`},
		// clamp with NaN value: pyMax(lo, pyMin(hi, NaN))
		// pyMin(1, NaN): NaN < 1 is false → NaN. pyMax(0, NaN): NaN > 0 false → NaN → null.
		{"core.math.clamp@1", in("value", `"nan"`, "min", `0`, "max", `1`), `null`},
	}
	for _, c := range cases {
		if got := runPure(t, c.id, c.inputs, nil); got != c.want {
			t.Errorf("non-finite %s(%v) = %s, want %s (must be null, never +Inf or panic)", c.id, c.inputs, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// get-field: phase 2 payload extractor — additional robustness paths
// ---------------------------------------------------------------------------

// TestProbe_GetField_NullIntermediatePaths — walking through null/scalar
// mid-path nodes and malformed dot paths.
func TestProbe_GetField_NullIntermediatePaths(t *testing.T) {
	cases := []struct {
		name   string
		record string
		path   string // bare path string, not JSON-quoted
		want   string
	}{
		{"null mid-path", `{"a":null}`, "a.b", `null`},
		{"number mid-path", `{"a":3}`, "a.sub", `null`},
		{"bool mid-path", `{"a":true}`, "a.sub", `null`},
		{"deeply nested null", `{"a":{"b":null}}`, "a.b.c", `null`},
		{"trailing dot on object", `{"a":{"b":1}}`, "a.", `null`},
		{"double dot", `{"a":{"b":1}}`, "a..b", `null`},
		{"leading dot", `{"a":1}`, ".a", `null`},
	}
	for _, c := range cases {
		pathJSON, _ := json.Marshal(c.path)
		if got := runPure(t, "core.data.get-field@1",
			in("record", c.record), cfg("path", string(pathJSON))); got != c.want {
			t.Errorf("get-field path-robustness %s = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestProbe_GetField_ConfigPathNull — config["path"] is JSON null.
// configStr(config, "path") → pyStrRaw(null) → "" → empty path → returns record.
func TestProbe_GetField_ConfigPathNull(t *testing.T) {
	record := `{"a":1}`
	got := runPure(t, "core.data.get-field@1", in("record", record), cfg("path", `null`))
	if got != record {
		t.Errorf("get-field config path=null: got %s, want %s (null path → empty → record itself)", got, record)
	}
}

// TestProbe_GetField_WhitespaceListIndex — Python int(part) strips
// whitespace; walkPath calls strconv.Atoi(strings.TrimSpace(part)).
// A path "tags. 1" with a space in the index part should still resolve.
// This tests a documented Python semantics detail.
func TestProbe_GetField_WhitespaceListIndex(t *testing.T) {
	// Note: the path is authored as a Go string; the config JSON encoding
	// wraps it. Since path splitting is on ".", "tags. 1" splits to
	// ["tags", " 1"], and TrimSpace(" 1") = "1" → valid index.
	pathJSON, _ := json.Marshal("tags. 1")
	record := `{"tags":["a","b","c"]}`
	got := runPure(t, "core.data.get-field@1", in("record", record), cfg("path", string(pathJSON)))
	if got != `"b"` {
		t.Errorf("get-field whitespace list index: got %s, want \"b\" (TrimSpace should allow \" 1\" as index 1)", got)
	}
}

// TestProbe_GetField_NegativeIndexEdge — walkPath allows negative indices
// (Python negative indexing). List length 3: index -3 is valid (first
// element), -4 is out-of-range.
func TestProbe_GetField_NegativeIndexEdge(t *testing.T) {
	record := `{"tags":["a","b","c"]}`
	cases := []struct {
		path string
		want string
	}{
		{"tags.-1", `"c"`},  // already in Forge's suite
		{"tags.-3", `"a"`},  // boundary: exactly the first element
		{"tags.-4", `null`}, // out of range after negative normalisation
	}
	for _, c := range cases {
		pathJSON, _ := json.Marshal(c.path)
		if got := runPure(t, "core.data.get-field@1", in("record", record), cfg("path", string(pathJSON))); got != c.want {
			t.Errorf("get-field negative index %q: got %s, want %s", c.path, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// set-field: additional robustness
// ---------------------------------------------------------------------------

// TestProbe_SetField_ConfigPathNull — config["path"] null → empty path
// → returns value (same as empty path contract).
func TestProbe_SetField_ConfigPathNull(t *testing.T) {
	got := runPure(t, "core.data.set-field@1",
		in("record", `{"a":1}`, "value", `"replaced"`),
		cfg("path", `null`))
	if got != `"replaced"` {
		t.Errorf("set-field config path=null: got %s, want \"replaced\" (null path → empty → return value)", got)
	}
}

// TestProbe_SetField_DeepNested — three-level path with existing siblings
// at each level should not clobber them.
func TestProbe_SetField_DeepNested(t *testing.T) {
	record := `{"a":{"b":{"c":1,"keep":2},"sibling":3},"top":4}`
	got := runPure(t, "core.data.set-field@1",
		in("record", record, "value", `99`),
		cfg("path", `"a.b.c"`))
	// We expect a.b.c=99, a.b.keep=2, a.sibling=3, top=4 preserved.
	// Parse and check — map iteration order is not guaranteed in JSON.
	var result map[string]any
	if err := json.Unmarshal([]byte(got), &result); err != nil {
		t.Fatalf("set-field deep-nested: invalid JSON result %s: %v", got, err)
	}
	a := result["a"].(map[string]any)
	b := a["b"].(map[string]any)
	if b["c"].(float64) != 99 {
		t.Errorf("set-field deep nested: a.b.c = %v, want 99", b["c"])
	}
	if b["keep"].(float64) != 2 {
		t.Errorf("set-field deep nested: a.b.keep = %v, want 2 (sibling must be preserved)", b["keep"])
	}
	if a["sibling"].(float64) != 3 {
		t.Errorf("set-field deep nested: a.sibling = %v, want 3", a["sibling"])
	}
	if result["top"].(float64) != 4 {
		t.Errorf("set-field deep nested: top = %v, want 4", result["top"])
	}
}

// ---------------------------------------------------------------------------
// cast.to-integer: -0 from Trunc
// ---------------------------------------------------------------------------

// TestProbe_ToInteger_NegZeroTrunc — Go's math.Trunc(-0.0) returns -0.0.
// jsonInt must normalise this to 0 on the wire.
// Also covers very small negative values where Trunc → -0.
func TestProbe_ToInteger_NegZeroTrunc(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`-0.0`, `0`},
		{`"-0.0"`, `0`}, // string "-0.0" → ParseFloat → -0.0 → Trunc → -0 → "0"
		{`-0.1`, `0`},   // Trunc(-0.1) = -0.0 in Go
		{`-0.9`, `0`},   // Trunc(-0.9) = -0.0 in Go
	}
	for _, c := range cases {
		if got := runPure(t, "core.cast.to-integer@1", in("value", c.input), nil); got != c.want {
			t.Errorf("to-integer(%s) = %s, want %s (-0 must never appear on integer output)", c.input, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// string.format: additional edge cases
// ---------------------------------------------------------------------------

// TestProbe_StringFormat_NullTemplate — template is JSON null.
// pyStr(null) → ""; fallback json.Marshal("") → `""`.
func TestProbe_StringFormat_NullTemplate(t *testing.T) {
	got := runPure(t, "core.string.format@1",
		in("template", `null`, "args", `{"a":"v"}`), nil)
	if got != `""` {
		t.Errorf("format null template = %s, want \"\" (null template coerces to empty string)", got)
	}
}

// TestProbe_StringFormat_DoubleClosingEscape — `}}` at end of template
// should output `}` without error.
func TestProbe_StringFormat_BraceEscapes(t *testing.T) {
	cases := []struct {
		name   string
		tmpl   string
		args   string
		want   string
	}{
		{"double close at end", `"value: {v}}"`, `{"v":"x"}`, `"value: x}"`},
		{"double open only", `"{{only}}"`, `{}`, `"{only}"`},
		{"both escapes", `"{{{v}}}"`, `{"v":"x"}`, `"{x}"`},
	}
	for _, c := range cases {
		got := runPure(t, "core.string.format@1",
			in("template", c.tmpl, "args", c.args), nil)
		if got != c.want {
			t.Errorf("format brace-escapes %s = %s, want %s", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// logic: -0 truthiness
// ---------------------------------------------------------------------------

// TestProbe_Logic_NegZeroFalsy — -0.0 must be falsy (== 0 numerically).
// pyTruthyRaw: float64(-0.0) != 0 is FALSE in Go (same bit pattern for
// comparison), so -0 is correctly falsy. This is pinned here as an
// explicit regression guard separate from Forge's combined suite.
func TestProbe_Logic_NegZeroFalsy(t *testing.T) {
	// -0 or true → true (because -0 falsy, true truthy → true)
	if got := runPure(t, "core.logic.or@1", in("a", `-0.0`, "b", `true`), nil); got != `true` {
		t.Errorf("or(-0, true) = %s, want true (-0 must be falsy)", got)
	}
	// -0 and true → false (because -0 falsy)
	if got := runPure(t, "core.logic.and@1", in("a", `-0.0`, "b", `true`), nil); got != `false` {
		t.Errorf("and(-0, true) = %s, want false (-0 must be falsy)", got)
	}
	// xor(-0, false) → false xor false → false
	if got := runPure(t, "core.logic.xor@1", in("a", `-0.0`, "b", `false`), nil); got != `false` {
		t.Errorf("xor(-0, false) = %s, want false (-0 falsy)", got)
	}
}

// ---------------------------------------------------------------------------
// list-at: additional index edge cases
// ---------------------------------------------------------------------------

// TestProbe_ListAt_LargeInt — a very large valid int64 index that is out
// of range for the list. Must return null, not panic or overflow.
func TestProbe_ListAt_LargeInt(t *testing.T) {
	if got := runPure(t, "core.data.list-at@1",
		in("list", `[1,2,3]`, "index", `9999999999`), nil); got != `null` {
		t.Errorf("list-at large index = %s, want null", got)
	}
}

// TestProbe_ListAt_NegativeIndex — negative indices are out-of-range by
// the explicit `idx < 0` guard in dataListAtFn (not Python's wrapping).
func TestProbe_ListAt_NegativeIndex(t *testing.T) {
	cases := []string{`-1`, `-2`, `-100`}
	for _, idx := range cases {
		if got := runPure(t, "core.data.list-at@1",
			in("list", `["a","b","c"]`, "index", idx), nil); got != `null` {
			t.Errorf("list-at negative index %s = %s, want null (no Python wrapping)", idx, got)
		}
	}
}

// ---------------------------------------------------------------------------
// configStr: nil config map guard
// ---------------------------------------------------------------------------

// TestProbe_ConfigStrNilMap — a node that uses config (get-field/set-field/
// aggregate) invoked with a nil config must not panic. configStr short-
// circuits on the nil map check in dataGetFieldFn (config is nil → configStr
// returns ""). Equivalent to "missing path config" — empty path returns record.
func TestProbe_ConfigStrNilMap(t *testing.T) {
	// aggregate with nil config: op defaults to "sum".
	if got := runPure(t, "core.data.aggregate@1", in("items", `[1,2,3]`), nil); got != `6` {
		t.Errorf("aggregate nil config = %s, want 6 (nil config → op default sum)", got)
	}
	// set-field with nil config: path defaults to "" → returns value.
	if got := runPure(t, "core.data.set-field@1",
		in("record", `{"a":1}`, "value", `"v"`), nil); got != `"v"` {
		t.Errorf("set-field nil config = %s, want \"v\" (nil config → empty path → value)", got)
	}
}

// ---------------------------------------------------------------------------
// math.min / math.max: missing / null / non-numeric coercions
// ---------------------------------------------------------------------------

func TestProbe_MinMaxCoercions(t *testing.T) {
	cases := []struct {
		id     string
		inputs map[string]json.RawMessage
		want   string
	}{
		// null input → coerces to default 0
		{"core.math.min@1", in("a", `null`, "b", `5`), `0`},
		{"core.math.max@1", in("a", `null`, "b", `-5`), `0`},
		// bool coercion: true→1, false→0
		{"core.math.min@1", in("a", `true`, "b", `2`), `1`},
		{"core.math.max@1", in("a", `false`, "b", `-1`), `0`},
		// object/array → coerces to default 0
		{"core.math.min@1", in("a", `[1,2]`, "b", `3`), `0`},
		{"core.math.max@1", in("a", `{"x":1}`, "b", `-1`), `0`},
	}
	for _, c := range cases {
		if got := runPure(t, c.id, c.inputs, nil); got != c.want {
			t.Errorf("%s coercion(%v) = %s, want %s", c.id, c.inputs, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// string.split: multi-char separator
// ---------------------------------------------------------------------------

func TestProbe_StringSplit_MultiChar(t *testing.T) {
	cases := []struct {
		value string
		sep   string
		want  string
	}{
		{`"a::b::c"`, `"::"`, `["a","b","c"]`},
		{`"one<br>two"`, `"<br>"`, `["one","two"]`},
		// separator longer than value → one-element list
		{`"ab"`, `"abc"`, `["ab"]`},
	}
	for _, c := range cases {
		if got := runPure(t, "core.string.split@1",
			in("value", c.value, "separator", c.sep), nil); got != c.want {
			t.Errorf("split multichar value=%s sep=%s = %s, want %s", c.value, c.sep, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Wired null vs missing — the critical distinction
// ---------------------------------------------------------------------------

// TestProbe_WiredNullVsMissing — a wired null must coerce like Python None
// (for _num: → default; for pyTruthy: → false; for list-at index: → null).
// A MISSING input also resolves to the default (same observable result for
// most coercions), but the path through the code is different.
// This test pins that both paths converge to the same result.
func TestProbe_WiredNullVsMissing(t *testing.T) {
	nodes := []struct {
		id        string
		portName  string
		wiredNull string // expected result when port carries JSON null
		missing   string // expected result when port is absent
	}{
		{"core.math.abs@1", "value", `0`, `0`},
		{"core.math.min@1", "a", `0`, `0`}, // null coerces to 0 = default
		{"core.math.max@1", "b", `0`, `0`},
		{"core.math.round@1", "value", `0`, `0`},
		{"core.math.floor@1", "value", `0`, `0`},
		{"core.math.ceil@1", "value", `0`, `0`},
		{"core.string.concat@1", "a", `"b"`, `"b"`},  // null→"", missing→"", concat with b="b"
		{"core.string.upper@1", "value", `""`, `""`},
		{"core.cast.to-boolean@1", "value", `false`, `false`},
		{"core.cast.to-float@1", "value", `0`, `0`},
	}
	for _, n := range nodes {
		var inputsNull, inputsMissing map[string]json.RawMessage
		if n.id == "core.string.concat@1" {
			inputsNull = in(n.portName, `null`, "b", `"b"`)
			inputsMissing = in("b", `"b"`)
		} else {
			inputsNull = in(n.portName, `null`)
			inputsMissing = in()
		}
		gotNull := runPure(t, n.id, inputsNull, nil)
		gotMissing := runPure(t, n.id, inputsMissing, nil)
		if gotNull != n.wiredNull {
			t.Errorf("%s wired null %s = %s, want %s", n.id, n.portName, gotNull, n.wiredNull)
		}
		if gotMissing != n.missing {
			t.Errorf("%s missing %s = %s, want %s", n.id, n.portName, gotMissing, n.missing)
		}
	}
}
