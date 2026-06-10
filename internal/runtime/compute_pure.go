package runtime

// Pure data-node registry tranche — ADR 003 §3.4 phase 0, issue #81.
//
// 27 executors: core.logic.{and,or,xor}, core.math.{abs,min,max,clamp,
// lerp,round,floor,ceil}, core.string.{concat,format,length,split,
// upper,lower}, core.cast.{to-string,to-integer,to-float,to-boolean},
// core.data.{get-field,set-field,list-length,list-at,list-append,
// aggregate}.
//
// The semantic source of truth is Blue's reference executor
// (Blue/src/blue/services/executor.py) plus the seeded signatures
// (stdlib_seeder.py). Two contracts from that pair are load-bearing
// everywhere below:
//
//   - Coercion never errors. Blue's `_num` / `bool()` / `_str` coerce
//     any JSON value (missing/null → the port's default); a data node
//     always produces a value. The error return on ComputeFn is kept
//     for genuinely broken artefacts only.
//   - Unwired ports receive their SIGNATURE DEFAULT. Blue's walker
//     (`_resolve_data_input`) injects `spec.default` when a port has
//     no inbound edge; Orion reproduces that inside each executor by
//     defaulting MISSING inputs (a wired null stays null and coerces
//     like Python None — which for `_num(None, d)` is also d, but for
//     list-at's strict int guard is "not an int" → null element).
//
// IEEE-754 signed zero is handled deliberately (the -0 hunt):
// integer-typed outputs (round/floor/ceil/to-integer) can never emit
// `-0` (Python ints carry no sign bit) — jsonInt normalises; min/max/
// clamp reproduce Python's first-argument-wins tie semantics, which is
// observable with signed zeros and which Go's math.Min/math.Max would
// NOT reproduce (they always prefer -0).

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// registerPureTranche adds the issue #81 executors to the registry.
// Called from NewComputeRegistry — the single seam the registry grows
// through.
func (r *ComputeRegistry) registerPureTranche() {
	// Logic — ports `a`, `b` (stdlib core.logic.*). Python truthiness
	// over any JSON value (executor.py `bool(i.get(...))`): null /
	// missing / 0 / -0 / "" / [] / {} are false, everything else true.
	r.fns["core.logic.and@1"] = logicBinary(func(a, b bool) bool { return a && b })
	r.fns["core.logic.or@1"] = logicBinary(func(a, b bool) bool { return a || b })
	r.fns["core.logic.xor@1"] = logicBinary(func(a, b bool) bool { return a != b })

	// Extended math — float in, float/integer out (stdlib core.math.*).
	r.fns["core.math.abs@1"] = mathAbsFn
	r.fns["core.math.min@1"] = mathMinFn
	r.fns["core.math.max@1"] = mathMaxFn
	r.fns["core.math.clamp@1"] = mathClampFn
	r.fns["core.math.lerp@1"] = mathLerpFn
	r.fns["core.math.round@1"] = mathRoundFn
	r.fns["core.math.floor@1"] = mathFloorFn
	r.fns["core.math.ceil@1"] = mathCeilFn

	// String — stdlib core.string.*.
	r.fns["core.string.concat@1"] = strConcatFn
	r.fns["core.string.format@1"] = strFormatFn
	r.fns["core.string.length@1"] = strLengthFn
	r.fns["core.string.split@1"] = strSplitFn
	r.fns["core.string.upper@1"] = strUpperFn
	r.fns["core.string.lower@1"] = strLowerFn

	// Cast — stdlib core.cast.*.
	r.fns["core.cast.to-string@1"] = castToStringFn
	r.fns["core.cast.to-integer@1"] = castToIntegerFn
	r.fns["core.cast.to-float@1"] = castToFloatFn
	r.fns["core.cast.to-boolean@1"] = castToBooleanFn

	// Data — stdlib core.data.*. get-field is the phase 2 payload
	// extractor (ADR 003 §3.3) — its dot-path walk is the most
	// load-bearing function in this tranche.
	r.fns["core.data.get-field@1"] = dataGetFieldFn
	r.fns["core.data.set-field@1"] = dataSetFieldFn
	r.fns["core.data.list-length@1"] = dataListLengthFn
	r.fns["core.data.list-at@1"] = dataListAtFn
	r.fns["core.data.list-append@1"] = dataListAppendFn
	r.fns["core.data.aggregate@1"] = dataAggregateFn
}

// ---------------------------------------------------------------------------
// Coercion helpers (executor.py _num / bool() / _str equivalents)
// ---------------------------------------------------------------------------

// pyNum mirrors Blue's `_num(value, default)`: missing or null → the
// default; numbers pass through; booleans coerce 1/0 (Python
// float(True) == 1.0); numeric strings parse (whitespace-trimmed, as
// Python float() strips); anything else — objects, lists, non-numeric
// strings — falls back to the default. Never errors.
func pyNum(inputs map[string]json.RawMessage, name string, def float64) float64 {
	raw, ok := inputs[name]
	if !ok {
		return def
	}
	return pyNumRaw(raw, def)
}

func pyNumRaw(raw json.RawMessage, def float64) float64 {
	// Decode through a pointer: encoding/json treats a JSON null as a
	// NO-OP success on a plain float64 (it would silently read as 0
	// instead of the port default — Python _num(None, d) returns d).
	var f *float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f == nil {
			return def // JSON null
		}
		return *f
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			return 1
		}
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f
		}
	}
	return def
}

// pyTruthy mirrors Python truthiness over a decoded JSON value:
// null/missing → false; bool → itself; number → != 0 (so -0 is false,
// NaN — unauthorable in JSON — would be true); string/array/object →
// non-empty. Used by the logic nodes and core.cast.to-boolean
// (seeder: "Truthiness check (0, ”, null -> false)").
func pyTruthy(inputs map[string]json.RawMessage, name string) bool {
	raw, ok := inputs[name]
	if !ok {
		return false
	}
	return pyTruthyRaw(raw)
}

func pyTruthyRaw(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return false // nil (JSON null) and anything unexpected
}

// pyStr mirrors Blue's `_str(value)`: missing/null → "", strings pass
// through verbatim. Non-string scalars and composites render as
// compact JSON ("true", "3.5", {"a":1}) — a deliberate, documented
// divergence from Python's repr (str(True) == "True",
// str({'a': 1}) == "{'a': 1}"): the Python forms are not portable and
// JSON is the only representation both runtimes share on the wire.
func pyStr(inputs map[string]json.RawMessage, name string) string {
	raw, ok := inputs[name]
	if !ok {
		return ""
	}
	return pyStrRaw(raw)
}

func pyStrRaw(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return ""
	}
	return buf.String()
}

// jsonNum marshals a float result. Non-finite values (authorable only
// via "inf"/"nan" strings through pyNum's ParseFloat) cannot travel as
// JSON numbers — they land as null rather than failing the compute.
func jsonNum(v float64) (json.RawMessage, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return json.RawMessage(`null`), nil
	}
	out, err := json.Marshal(v)
	return out, err
}

// jsonInt marshals an INTEGER-typed result (round/floor/ceil/
// to-integer outputs are integers in Blue's signatures). A Python int
// carries no sign bit, so `-0` must never appear on the wire — Go's
// math.Ceil(-0.3) and math.RoundToEven(-0.5) both return -0.0, which
// json.Marshal would render as "-0". The comparison `v == 0` is true
// for both zeros; reassigning the literal strips the sign.
func jsonInt(v float64) (json.RawMessage, error) {
	if v == 0 {
		v = 0
	}
	return jsonNum(v)
}

// pyMin / pyMax mirror Python's two-argument min()/max(): the FIRST
// argument wins ties (CPython keeps the incumbent unless the candidate
// strictly compares). Observable with signed zeros —
// min(-0.0, 0.0) == -0.0 but min(0.0, -0.0) == 0.0 — semantics Go's
// math.Min/math.Max do NOT have (they always prefer -0 / +0).
func pyMin(a, b float64) float64 {
	if b < a {
		return b
	}
	return a
}

func pyMax(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

// ---------------------------------------------------------------------------
// Logic
// ---------------------------------------------------------------------------

func logicBinary(op func(a, b bool) bool) ComputeFn {
	return func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
		out, _ := json.Marshal(op(pyTruthy(inputs, "a"), pyTruthy(inputs, "b")))
		return out, nil
	}
}

// ---------------------------------------------------------------------------
// Extended math
// ---------------------------------------------------------------------------

// mathAbsFn — core.math.abs@1, port `value`. math.Abs clears the sign
// bit, so abs(-0) is +0, matching Python's abs(-0.0) == 0.0.
func mathAbsFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	return jsonNum(math.Abs(pyNum(inputs, "value", 0)))
}

// mathMinFn / mathMaxFn — ports `a`, `b`. First-argument-wins ties
// (see pyMin/pyMax) — the artefact's named wiring makes "first" the
// declared `a` port, never edge order.
func mathMinFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	return jsonNum(pyMin(pyNum(inputs, "a", 0), pyNum(inputs, "b", 0)))
}

func mathMaxFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	return jsonNum(pyMax(pyNum(inputs, "a", 0), pyNum(inputs, "b", 0)))
}

// mathClampFn — ports `value`, `min` (default 0), `max` (default 1).
// Exact composition from executor.py: max(lo, min(hi, v)) — note the
// composition is what defines behaviour when lo > hi (the lower bound
// wins) and how signed zeros propagate; do not "simplify" it.
func mathClampFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	v := pyNum(inputs, "value", 0)
	lo := pyNum(inputs, "min", 0)
	hi := pyNum(inputs, "max", 1)
	return jsonNum(pyMax(lo, pyMin(hi, v)))
}

// mathLerpFn — ports `a` (default 0), `b` (default 1), `alpha`
// (default 0.5). a + (b-a)*alpha, NOT clamped: alpha outside [0,1]
// extrapolates, exactly as the reference does.
func mathLerpFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	a := pyNum(inputs, "a", 0)
	b := pyNum(inputs, "b", 1)
	alpha := pyNum(inputs, "alpha", 0.5)
	return jsonNum(a + (b-a)*alpha)
}

// mathRoundFn — port `value`, INTEGER output. Banker's rounding
// (round-half-to-even), explicitly documented in the seeder ("via
// Python's built-in"): round(0.5)=0, round(1.5)=2, round(2.5)=2.
// math.RoundToEven matches; jsonInt strips the -0 that
// RoundToEven(-0.5) produces (Python yields int 0).
func mathRoundFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	return jsonInt(math.RoundToEven(pyNum(inputs, "value", 0)))
}

// mathFloorFn — port `value`, INTEGER output, rounds toward -inf.
func mathFloorFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	return jsonInt(math.Floor(pyNum(inputs, "value", 0)))
}

// mathCeilFn — port `value`, INTEGER output, rounds toward +inf.
// math.Ceil(-0.3) is -0.0 in Go; Python's math.ceil gives int 0 —
// jsonInt normalises.
func mathCeilFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	return jsonInt(math.Ceil(pyNum(inputs, "value", 0)))
}

// ---------------------------------------------------------------------------
// String
// ---------------------------------------------------------------------------

// strConcatFn — ports `a`, `b` (defaults ""). _str coercion: null →
// "", numbers/bools render (so concat("score: ", 3.5) works).
func strConcatFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	out, err := json.Marshal(pyStr(inputs, "a") + pyStr(inputs, "b"))
	return out, err
}

// strFormatFn — ports `template`, `args`. Interpolates {name}
// placeholders from a JSON object. Failure contract mirrors
// executor.py exactly: args not an object, a missing key (KeyError),
// or any malformed/unsupported placeholder (positional {}, format
// specs {x:.2f}, attribute/index access — ValueError/IndexError
// territory) returns the template UNCHANGED, never an error.
func strFormatFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	template := pyStr(inputs, "template")
	fallback, err := json.Marshal(template)
	if err != nil {
		return nil, err
	}
	argsRaw, ok := inputs["args"]
	if !ok {
		return fallback, nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(argsRaw, &args); err != nil || args == nil {
		return fallback, nil
	}
	formatted, ok := pyFormat(template, args)
	if !ok {
		return fallback, nil
	}
	out, err := json.Marshal(formatted)
	return out, err
}

// pyFormat scans the template for {name} placeholders ({{ and }} are
// literal-brace escapes, as in Python). Returns ok=false on the first
// anomaly so the caller can fall back to the raw template.
func pyFormat(template string, args map[string]json.RawMessage) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(template); {
		switch template[i] {
		case '{':
			if i+1 < len(template) && template[i+1] == '{' {
				b.WriteByte('{')
				i += 2
				continue
			}
			end := strings.IndexByte(template[i+1:], '}')
			if end < 0 {
				return "", false // unmatched '{' — Python ValueError
			}
			name := template[i+1 : i+1+end]
			if name == "" || strings.ContainsAny(name, ":!.[{") {
				// Positional ({}), format-spec ({x:.2f}), conversion
				// ({x!r}) and attribute/index forms are full Python
				// format machinery — out of the {name} contract the
				// seeder declares. Fall back like Python's except path.
				return "", false
			}
			raw, ok := args[name]
			if !ok {
				return "", false // KeyError
			}
			b.WriteString(pyStrRaw(raw))
			i += end + 2
		case '}':
			if i+1 < len(template) && template[i+1] == '}' {
				b.WriteByte('}')
				i += 2
				continue
			}
			return "", false // lone '}' — Python ValueError
		default:
			b.WriteByte(template[i])
			i++
		}
	}
	return b.String(), true
}

// strLengthFn — port `value`, INTEGER output `count`. Counts Unicode
// code points (Python len over str), not bytes.
func strLengthFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	out, err := json.Marshal(utf8.RuneCountInString(pyStr(inputs, "value")))
	return out, err
}

// strSplitFn — ports `value`, `separator` (default ","). An empty or
// missing separator falls back to "," (executor.py: `_str(...) or
// ","` — Python's str.split("") would raise). Splitting "" yields
// [""], as both languages do.
func strSplitFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	sep := pyStr(inputs, "separator")
	if sep == "" {
		sep = ","
	}
	out, err := json.Marshal(strings.Split(pyStr(inputs, "value"), sep))
	return out, err
}

func strUpperFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	out, err := json.Marshal(strings.ToUpper(pyStr(inputs, "value")))
	return out, err
}

func strLowerFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	out, err := json.Marshal(strings.ToLower(pyStr(inputs, "value")))
	return out, err
}

// ---------------------------------------------------------------------------
// Cast
// ---------------------------------------------------------------------------

func castToStringFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	out, err := json.Marshal(pyStr(inputs, "value"))
	return out, err
}

// castToIntegerFn — port `value`, INTEGER output `result` (+ `ok`,
// see the multi-output note below). Python int() semantics: numbers
// truncate toward zero (int(3.9)=3, int(-3.9)=-3); booleans coerce
// 1/0 (bool subclasses int); strings parse as base-10 integers after
// whitespace trim — int("3.5") is a ValueError, so "3.5" → 0;
// null/missing/composites → 0.
//
// Multi-output note: the signature declares a second `ok` boolean
// output. The dataflow substrate stores ONE value per node and the
// artefact does not carry edge from_port, so no node's secondary
// output is independently addressable yet (a substrate-wide phase 0/1
// gap, not a restriction of this executor) — the primary `result` is
// what the node's downstream edges receive.
func castToIntegerFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	raw, ok := inputs["value"]
	if !ok {
		// Python: str(None) == "None" → ValueError → result 0.
		return json.RawMessage(`0`), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(`0`), nil
	}
	switch t := v.(type) {
	case float64:
		return jsonInt(math.Trunc(t))
	case bool:
		if t {
			return json.RawMessage(`1`), nil
		}
		return json.RawMessage(`0`), nil
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return json.Marshal(n)
		}
	}
	return json.RawMessage(`0`), nil
}

// castToFloatFn — port `value`, FLOAT output `result` (+ `ok`, same
// multi-output note as to-integer). Python float() semantics —
// numbers pass, booleans 1/0, strings ParseFloat (so "-0.0" keeps its
// IEEE sign, as Python float("-0.0") does), failures → 0.0.
func castToFloatFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	raw, ok := inputs["value"]
	if !ok {
		return json.RawMessage(`0`), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(`0`), nil
	}
	switch t := v.(type) {
	case float64:
		return jsonNum(t)
	case bool:
		if t {
			return json.RawMessage(`1`), nil
		}
		return json.RawMessage(`0`), nil
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return jsonNum(f)
		}
	}
	return json.RawMessage(`0`), nil
}

func castToBooleanFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	out, err := json.Marshal(pyTruthy(inputs, "value"))
	return out, err
}

// ---------------------------------------------------------------------------
// Data
// ---------------------------------------------------------------------------

// dataGetFieldFn — core.data.get-field@1, port `record`, config
// `path` (dot path, e.g. "user.name"). THE phase 2 payload extractor
// (ADR 003 §3.3): a quasar.* platform event lands as a JSON object
// and this node pulls fields out of it. Faithful to executor.py's
// _walk_path: empty path returns the record itself; objects index by
// key (missing key → null); lists index by int(part) WITH Python
// negative indexing (lst[-1] is the last element — executor.py only
// catches ValueError/IndexError, and negative indices raise neither);
// any scalar mid-walk → null. Never errors.
func dataGetFieldFn(inputs, config map[string]json.RawMessage) (json.RawMessage, error) {
	path := configStr(config, "path")
	var record any
	if raw, ok := inputs["record"]; ok {
		// A decode failure leaves record nil — walk yields null.
		_ = json.Unmarshal(raw, &record)
	}
	return json.Marshal(walkPath(record, path))
}

func walkPath(cursor any, path string) any {
	if path == "" {
		return cursor
	}
	for _, part := range strings.Split(path, ".") {
		switch c := cursor.(type) {
		case map[string]any:
			cursor = c[part] // missing key → nil (dict.get)
		case []any:
			// Python int(part) strips whitespace; negative indices are
			// valid Python list access (lst[-1] == last element).
			idx, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				return nil
			}
			if idx < 0 {
				idx += len(c)
			}
			if idx < 0 || idx >= len(c) {
				return nil
			}
			cursor = c[idx]
		default:
			return nil
		}
	}
	return cursor
}

// dataSetFieldFn — ports `record`, `value`, config `path`. Returns a
// NEW object with the field at the dot path replaced (executor.py
// _set_path): empty path returns the value itself; a non-object
// record is replaced by {}; non-object intermediates are overwritten
// by fresh objects on the way down.
func dataSetFieldFn(inputs, config map[string]json.RawMessage) (json.RawMessage, error) {
	path := configStr(config, "path")
	var record, value any
	if raw, ok := inputs["record"]; ok {
		_ = json.Unmarshal(raw, &record)
	}
	if raw, ok := inputs["value"]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return json.Marshal(setPath(record, path, value))
}

func setPath(record any, path string, value any) any {
	if path == "" {
		return value
	}
	parts := strings.Split(path, ".")
	root := map[string]any{}
	if m, ok := record.(map[string]any); ok {
		// The decode above is private to this call, so reusing the
		// nested values is safe — same observable result as Python's
		// shallow dict(record) copy + in-place nested writes.
		for k, v := range m {
			root[k] = v
		}
	}
	cursor := root
	for _, part := range parts[:len(parts)-1] {
		next, ok := cursor[part].(map[string]any)
		if !ok {
			next = map[string]any{}
		}
		cursor[part] = next
		cursor = next
	}
	cursor[parts[len(parts)-1]] = value
	return root
}

// dataListLengthFn — port `list`, INTEGER output `count`. Non-list
// (incl. strings — Python isinstance(str, list) is false) → 0.
func dataListLengthFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	var lst []json.RawMessage
	if raw, ok := inputs["list"]; ok {
		if err := json.Unmarshal(raw, &lst); err != nil {
			lst = nil
		}
	}
	return json.Marshal(len(lst))
}

// dataListAtFn — ports `list`, `index` (signature default 0), output
// `element`. Mirrors executor.py's STRICT guard: the wired index must
// be a JSON integer — a float-typed value ("1.0", "1e2" — floats
// after json.loads in Python too) yields null, out-of-range
// (including negative — the reference guards 0 <= idx < len
// explicitly, no Python negative indexing here) yields null. A
// MISSING index gets the signature default 0 (Blue's walker injects
// it); a wired null is Python None → not an int → null. One reading:
// JSON true/false decode to Python bool, which subclasses int, so the
// reference would index lst[1]/lst[0]; that is an artefact of
// Python's type lattice, not the seeder's "index (0-based)" contract
// — booleans yield null here (divergence flagged in the PR).
func dataListAtFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	var lst []json.RawMessage
	if raw, ok := inputs["list"]; ok {
		if err := json.Unmarshal(raw, &lst); err != nil {
			return json.RawMessage(`null`), nil
		}
	} else {
		return json.RawMessage(`null`), nil
	}

	idx := 0
	if raw, ok := inputs["index"]; ok {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return json.RawMessage(`null`), nil
		}
		num, isNum := v.(json.Number)
		if !isNum {
			return json.RawMessage(`null`), nil
		}
		n, err := num.Int64() // "1.0"/"1e2" fail — float, not int
		if err != nil {
			return json.RawMessage(`null`), nil
		}
		idx = int(n)
	}

	if idx < 0 || idx >= len(lst) {
		return json.RawMessage(`null`), nil
	}
	return lst[idx], nil
}

// dataListAppendFn — ports `list`, `element`. Non-list/missing list
// starts from []; a missing element appends null (Python None).
func dataListAppendFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	var lst []json.RawMessage
	if raw, ok := inputs["list"]; ok {
		if err := json.Unmarshal(raw, &lst); err != nil {
			lst = nil
		}
	}
	element := json.RawMessage(`null`)
	if raw, ok := inputs["element"]; ok {
		element = raw
	}
	if lst == nil {
		lst = []json.RawMessage{}
	}
	return json.Marshal(append(lst, element))
}

// dataAggregateFn — port `items`, config `op` (sum | avg | min | max
// | count, default "sum"), FLOAT output `result`. Non-list or empty
// list → 0 (for every op, including count — executor.py's early
// return). Items coerce through _num, so non-numeric entries count
// as 0. min/max fold left with first-wins ties, preserving Python's
// min()/max() signed-zero behaviour over the list order. Unknown op
// → 0.
func dataAggregateFn(inputs, config map[string]json.RawMessage) (json.RawMessage, error) {
	op := configStr(config, "op")
	if op == "" {
		op = "sum"
	}
	var items []json.RawMessage
	if raw, ok := inputs["items"]; ok {
		if err := json.Unmarshal(raw, &items); err != nil {
			items = nil
		}
	}
	if len(items) == 0 {
		return json.RawMessage(`0`), nil
	}
	numbers := make([]float64, len(items))
	for i, raw := range items {
		numbers[i] = pyNumRaw(raw, 0)
	}
	switch op {
	case "sum", "avg":
		sum := 0.0
		for _, n := range numbers {
			sum += n
		}
		if op == "avg" {
			return jsonNum(sum / float64(len(numbers)))
		}
		return jsonNum(sum)
	case "min":
		acc := numbers[0]
		for _, n := range numbers[1:] {
			acc = pyMin(acc, n)
		}
		return jsonNum(acc)
	case "max":
		acc := numbers[0]
		for _, n := range numbers[1:] {
			acc = pyMax(acc, n)
		}
		return jsonNum(acc)
	case "count":
		return jsonNum(float64(len(items)))
	}
	return json.RawMessage(`0`), nil
}

// configStr reads a string config key (executor.py reads config via
// _str — null/missing → "").
func configStr(config map[string]json.RawMessage, key string) string {
	raw, ok := config[key]
	if !ok {
		return ""
	}
	return pyStrRaw(raw)
}
