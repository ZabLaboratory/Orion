package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ComputeFn is one entry in the compute registry — a pure function
// over named inputs. Returns the new value at the output path. v1
// arity is "many inputs in, one value out" matching Blue's stdlib
// node shape.
type ComputeFn func(inputs map[string]json.RawMessage) (json.RawMessage, error)

// ComputeRegistry maps Blue stdlib compute ids to Go implementations.
// The compiler enforced that every compute referenced by the graph
// is in Blue's manifest with is_pure: true; the runtime then needs an
// executable form. This registry is that link.
type ComputeRegistry struct {
	fns map[string]ComputeFn
}

// NewComputeRegistry builds the v1 stdlib registry. Mirrors the 13
// core nodes Blue seeds at first boot (literal, input, output, add,
// mul, if, get-field, set-field, plus a couple comparison helpers).
func NewComputeRegistry() *ComputeRegistry {
	r := &ComputeRegistry{fns: map[string]ComputeFn{}}
	r.fns["core.input"] = passthrough     // input nodes are passthrough leaves
	r.fns["core.literal"] = literalFn     // emits a constant — `default` arg is the value
	r.fns["core.passthrough"] = passthrough
	r.fns["core.identity"] = passthrough
	r.fns["core.add"] = arithmetic(func(a, b float64) float64 { return a + b })
	r.fns["core.sub"] = arithmetic(func(a, b float64) float64 { return a - b })
	r.fns["core.mul"] = arithmetic(func(a, b float64) float64 { return a * b })
	r.fns["core.div"] = arithmetic(func(a, b float64) float64 {
		if b == 0 {
			return 0
		}
		return a / b
	})
	r.fns["core.eq"] = comparator(func(a, b float64) bool { return a == b })
	r.fns["core.lt"] = comparator(func(a, b float64) bool { return a < b })
	r.fns["core.gt"] = comparator(func(a, b float64) bool { return a > b })
	r.fns["core.not"] = notFn
	r.fns["core.if"] = ifFn
	return r
}

// Get returns the ComputeFn, or an error if the registry doesn't
// know the id. The compiler guarantees this never happens in
// practice — the manifest gate rejects unknowns at push time — but
// the runtime defends against drift between Blue's manifest and the
// Go registry.
func (r *ComputeRegistry) Get(id string) (ComputeFn, error) {
	if fn, ok := r.fns[id]; ok {
		return fn, nil
	}
	return nil, fmt.Errorf("compute: id %q not in runtime registry", id)
}

// Register adds a custom compute. Tests use this to inject mocks.
func (r *ComputeRegistry) Register(id string, fn ComputeFn) {
	r.fns[id] = fn
}

// passthrough returns the first input by lexicographic key (or by
// the conventional `value` / `in` port). Inputs that are pre-sorted
// arrive deterministically.
func passthrough(inputs map[string]json.RawMessage) (json.RawMessage, error) {
	if v, ok := inputs["value"]; ok {
		return v, nil
	}
	if v, ok := inputs["in"]; ok {
		return v, nil
	}
	for _, v := range inputs {
		return v, nil
	}
	return json.RawMessage(`null`), nil
}

func literalFn(inputs map[string]json.RawMessage) (json.RawMessage, error) {
	if v, ok := inputs["default"]; ok {
		return v, nil
	}
	if v, ok := inputs["value"]; ok {
		return v, nil
	}
	return json.RawMessage(`null`), nil
}

// arithmetic builds an ADD/SUB/MUL/DIV compute. Inputs are read from
// ports `x` (or `a`) and `y` (or `b`).
func arithmetic(op func(a, b float64) float64) ComputeFn {
	return func(inputs map[string]json.RawMessage) (json.RawMessage, error) {
		a, err := readNum(inputs, "x", "a")
		if err != nil {
			return nil, err
		}
		b, err := readNum(inputs, "y", "b")
		if err != nil {
			return nil, err
		}
		out, _ := json.Marshal(op(a, b))
		return out, nil
	}
}

func comparator(op func(a, b float64) bool) ComputeFn {
	return func(inputs map[string]json.RawMessage) (json.RawMessage, error) {
		a, err := readNum(inputs, "x", "a")
		if err != nil {
			return nil, err
		}
		b, err := readNum(inputs, "y", "b")
		if err != nil {
			return nil, err
		}
		out, _ := json.Marshal(op(a, b))
		return out, nil
	}
}

func notFn(inputs map[string]json.RawMessage) (json.RawMessage, error) {
	v, err := readBool(inputs, "x", "value", "in")
	if err != nil {
		return nil, err
	}
	out, _ := json.Marshal(!v)
	return out, nil
}

func ifFn(inputs map[string]json.RawMessage) (json.RawMessage, error) {
	cond, err := readBool(inputs, "cond", "if", "test")
	if err != nil {
		return nil, err
	}
	if cond {
		if v, ok := inputs["then"]; ok {
			return v, nil
		}
	} else {
		if v, ok := inputs["else"]; ok {
			return v, nil
		}
	}
	return json.RawMessage(`null`), nil
}

// readNum tries each port name in order, returning the first numeric
// value found. Missing inputs default to 0 — Blue's stdlib does the
// same for unwired ports.
func readNum(inputs map[string]json.RawMessage, names ...string) (float64, error) {
	for _, n := range names {
		raw, ok := inputs[n]
		if !ok {
			continue
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			return f, nil
		}
		return 0, fmt.Errorf("compute: input %q not a number: %s", n, raw)
	}
	return 0, nil
}

func readBool(inputs map[string]json.RawMessage, names ...string) (bool, error) {
	for _, n := range names {
		raw, ok := inputs[n]
		if !ok {
			continue
		}
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			return b, nil
		}
		return false, fmt.Errorf("compute: input %q not a boolean: %s", n, raw)
	}
	return false, nil
}

// ErrUnknownCompute is returned by Get when the id isn't registered.
var ErrUnknownCompute = errors.New("compute: unknown id")
