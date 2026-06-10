package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// ComputeFn is one entry in the compute registry — a pure function
// over named inputs plus the node's authored config (issue #81,
// mirroring Blue's handler(inputs, config, state) contract in
// executor.py — config-bearing pure nodes like core.data.get-field
// read `config.path`). Returns the new value at the output path. v1
// arity is "many inputs in, one value out" matching Blue's stdlib
// node shape; config is nil for config-less nodes and pre-#81
// artefacts.
type ComputeFn func(inputs, config map[string]json.RawMessage) (json.RawMessage, error)

// ComputeRegistry maps Blue stdlib compute ids to Go implementations.
// The compiler enforced that every compute referenced by the graph
// is in Blue's manifest with is_pure: true; the runtime then needs an
// executable form. This registry is that link.
type ComputeRegistry struct {
	fns map[string]ComputeFn
}

// NewComputeRegistry builds the v1 stdlib registry. Every entry is
// keyed by the qualified id the compiler emits and Blue's manifest
// carries — `namespace.name@version` (ADR 004 §7.3, Orion#38). This is
// the same string as `GraphNode.Compute` (`compile.go`, `Compute:
// n.Compute`) and Blue's manifest key (`compute_manifest.py`,
// `node_id = f"{namespace}.{name}@{version}"`), so `cmpReg.Get(node.
// Compute)` resolves by construction.
//
// The *authoritative* set is Blue's seeded stdlib
// (`Blue/src/blue/services/stdlib_seeder.py`, `_CORE_NODES`). Only the
// nodes the reactive loop actually executes (`Kind != "input"`, i.e.
// computed + output sinks) need an entry. `core.input@1` and
// `core.literal@1` are deliberately absent: the former's leaf is
// adapter-written, the latter is seeded into `graph.Defaults` at
// compile time and lands `Kind == "input"` — both are skipped by
// `recompute` (`scene.go`). The set will grow with the producer (Prism)
// — this constructor is the single seam to extend.
func NewComputeRegistry() *ComputeRegistry {
	r := &ComputeRegistry{fns: map[string]ComputeFn{}}

	// Math — ports `a`, `b` (stdlib `core.math.*`).
	r.fns["core.math.add@1"] = arithmetic(func(a, b float64) float64 { return a + b })
	r.fns["core.math.sub@1"] = arithmetic(func(a, b float64) float64 { return a - b })
	r.fns["core.math.mul@1"] = arithmetic(func(a, b float64) float64 { return a * b })
	r.fns["core.math.div@1"] = arithmetic(func(a, b float64) float64 {
		if b == 0 {
			return 0
		}
		return a / b
	})
	r.fns["core.math.mod@1"] = arithmetic(math.Mod)

	// Comparison — ports `a`, `b` (stdlib `core.compare.*`).
	r.fns["core.compare.equal@1"] = comparator(func(a, b float64) bool { return a == b })
	r.fns["core.compare.not-equal@1"] = comparator(func(a, b float64) bool { return a != b })
	r.fns["core.compare.less-than@1"] = comparator(func(a, b float64) bool { return a < b })
	r.fns["core.compare.less-equal@1"] = comparator(func(a, b float64) bool { return a <= b })
	r.fns["core.compare.greater-than@1"] = comparator(func(a, b float64) bool { return a > b })
	r.fns["core.compare.greater-equal@1"] = comparator(func(a, b float64) bool { return a >= b })

	// Logic — port `a` (stdlib `core.logic.not`).
	r.fns["core.logic.not@1"] = notFn

	// Flow — ports `condition`, `when_true`, `when_false`
	// (stdlib `core.flow.select`).
	r.fns["core.flow.select@1"] = selectFn

	// Output sink — passthrough to its `Path` leaf (port `value`).
	// ADR 004 §7.3 Decision B(a): registered as a passthrough so the
	// existing `Kind != "input"` → Get → Set(Path) loop writes the
	// single inbound value to the leaf with zero change to `recompute`.
	r.fns["core.output@1"] = passthrough

	// Pure data-node tranche — logic, extended math, string, cast,
	// data (ADR 003 §3.4 phase 0, issue #81). compute_pure.go.
	r.registerPureTranche()

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

// IDs returns the set of registered compute ids. The conformance matrix
// (ADR 003 §6 criterion 1) uses it to prove the declared
// data-layer-served node set matches what the registry actually
// installs — so a node claimed "served by the compute registry" that
// nobody registered fails CI, not air.
func (r *ComputeRegistry) IDs() []string {
	out := make([]string, 0, len(r.fns))
	for id := range r.fns {
		out = append(out, id)
	}
	return out
}

// passthrough returns the node's single inbound value. Since issue #79
// (ADR 003 §3.1.1) `gatherInputs` delivers a compiled sink's one
// upstream under its DECLARED port name — the stdlib `value` input —
// so the name chain is no longer load-bearing for compiled artefacts.
// It survives as the deterministic fallback for pre-#79 persisted
// graphs (positional `a`) and for any port-convention drift: `a`,
// then `value` / `in`, then any remaining input, else `null`. A
// `core.output@1` sink has exactly one inbound edge, so the choice is
// unambiguous in practice.
func passthrough(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	for _, name := range []string{"a", "value", "in"} {
		if v, ok := inputs[name]; ok {
			return v, nil
		}
	}
	for _, v := range inputs {
		return v, nil
	}
	return json.RawMessage(`null`), nil
}

// arithmetic builds an ADD/SUB/MUL/DIV/MOD compute. Inputs are read from
// ports `x` (or `a`) and `y` (or `b`).
func arithmetic(op func(a, b float64) float64) ComputeFn {
	return func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
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
	return func(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
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

// notFn implements `core.logic.not@1`. The stdlib declares a single
// `a` port (`stdlib_seeder.py`); since issue #79 gatherInputs delivers
// the upstream under that declared name (which coincides with the
// positional fallback pre-#79 artefacts get). The extra names are
// tolerated, non-load-bearing fallbacks.
func notFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	v, err := readBool(inputs, "a", "x", "value", "in")
	if err != nil {
		return nil, err
	}
	out, _ := json.Marshal(!v)
	return out, nil
}

// selectFn implements `core.flow.select@1` (stdlib `core.flow.select`,
// `stdlib_seeder.py`): return `when_true` if `condition` is true, else
// `when_false`. Pure data, no exec pin. Since issue #79 (ADR 003
// §3.1.1) gatherInputs delivers each upstream under its DECLARED
// stdlib port name (`condition`/`when_true`/`when_false`) regardless
// of edge order — the primary names below are the load-bearing path.
// The positional names (`a`/`b`/`c`) remain only as the fallback for
// pre-#79 persisted artefacts, whose edges arrived zipped in authored
// order condition,when_true,when_false (ADR 004 §7.2).
func selectFn(inputs, _ map[string]json.RawMessage) (json.RawMessage, error) {
	cond, err := readBool(inputs, "condition", "a", "cond")
	if err != nil {
		return nil, err
	}
	if cond {
		for _, name := range []string{"when_true", "b", "then"} {
			if v, ok := inputs[name]; ok {
				return v, nil
			}
		}
	} else {
		for _, name := range []string{"when_false", "c", "else"} {
			if v, ok := inputs[name]; ok {
				return v, nil
			}
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
