package conformance_test

import (
	"sort"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/conformance"
)

// The exec-port-parity gate (Conduit, contract-port-parity-alignment).
//
// The class of bug this gate closes: a port or config NAME hardcoded by
// the Orion runtime that does NOT exist in the Blue seed signature the
// node is authored against. When the two drift, a node authored in Prism
// (against the seed) compiles, pushes, and runs — but the runtime reads a
// pin nobody wired and fires a pin nobody listens to, so the node fails
// SILENTLY on air. The campaign found six such drifts (for-each `list` vs
// seed `items`, gate out `exit` vs `then`, print `message` vs `value`,
// variable.set/get config `name` vs `variable`, source.read config
// `source` vs `source_id`, http output `response` vs `body`).
//
// What this asserts: for every node id the runtime serves, every
// port/config string the runtime hardcodes (read on a data pull, fired on
// a Next pin, or read from config) MUST exist in that node's seed
// signature (internal/conformance/signatures.json, vendored from
// stdlib_seeder.py). The signature is the user-facing contract; the
// runtime is the side that must conform.
//
// The runtime-side strings are listed EXPLICITLY in runtimeContract below,
// pinned and reviewed. This is deliberate: the gate proves the pinned
// contract matches BOTH the seed (this test) AND the live runtime — the
// latter via the existing conformance matrix
// (TestConformance_EveryExecOpExecutes drives each op through a real
// Scene, so a pinned-but-unread string would make that probe go dark).
// Together they sandwich the runtime's real behaviour between the seed and
// an executed proof.
//
// PROOF IT CATCHES THE DRIFT: flip any entry below back to its pre-fix
// name (e.g. print "value" → "message", gate "then" → "exit", source
// "source_id" → "source") and this test fails with a clear
// "<id>: runtime <kind> %q absent from seed signature" — exactly the
// drift the live exposed.

// runtimeStrings is one node's hardcoded runtime vocabulary.
type runtimeStrings struct {
	inputs  []string
	outputs []string
	config  []string
}

// runtimeContract pins, per served node id, every port/config name the
// Orion runtime hardcodes. Sources are cited so a reviewer can re-verify
// against the code. Internal pins (the `__effect_complete` re-entry pin,
// the `__nodestate` leaf) are NOT seed ports and are intentionally omitted
// — they are runtime-internal, never authored.
var runtimeContract = map[string]runtimeStrings{
	// --- exec entrypoints (exec.go / exec_partition.go) ---------------
	"core.event.on-start@1": {outputs: []string{"then"}},
	"core.event.on-tick@1":  {outputs: []string{"then", "delta_seconds"}},
	"core.event.on-event@1": {outputs: []string{"then"}, config: []string{"event_name"}},
	// on-platform-event (ADR 013): exec out `then` + data-out `payload`;
	// config platform/channel/event_type → the compiler canonicalises them
	// into the observed `__inputs.platform.*` leaf (ExecEntry.Event), the
	// runtime reads no config string of its own (the leaf is on the entry).
	"core.event.on-platform-event@1": {outputs: []string{"then", "payload"}, config: []string{"platform", "channel", "event_type"}},
	// on-call (Orion #209, Blue ADR 008 §3.2): exec out `then` + data-out
	// `payload` (the operator-call route binds the request payload here,
	// FireOnCall). The runtime reads no config string of its own — the
	// `entrypoint` name is the entry's addressing key (resolveEntry), and
	// `ui` is published verbatim by the surface, never read by the runtime.
	"core.operator.on-call@1": {outputs: []string{"then", "payload"}},
	// await-value (Orion #209, Blue ADR 008 §3.3): exec in `in` + exec out
	// `then`, data-out `value` (the resolved value is bound here on resume),
	// config `await_name`/`value_type` (read by execOperatorAwait/resolve);
	// `ui` is published, not read.
	"core.operator.await-value@1": {outputs: []string{"then", "value"}, config: []string{"await_name", "value_type"}},

	// --- flow control (exec_interpreter.go) ---------------------------
	// branch: pullBool "condition"; fires "true"/"false".
	"core.flow.branch@1": {inputs: []string{"condition"}, outputs: []string{"true", "false"}},
	// sequence: fires then_0..then_2 (the seed's three pins).
	"core.flow.sequence@1": {outputs: []string{"then_0", "then_1", "then_2"}},
	// gate: enters enter/open/close/toggle; fires "then" (execGate).
	"core.flow.gate@1": {inputs: []string{"enter", "open", "close", "toggle", "start_closed"}, outputs: []string{"then"}},
	// for-loop: pullInt first/last; fires body/completed; binds index.
	"core.flow.for-loop@1": {inputs: []string{"first", "last"}, outputs: []string{"body", "completed", "index"}},
	// for-each: pullArray "items"; fires body/completed; binds element/index.
	"core.flow.for-each@1": {inputs: []string{"items"}, outputs: []string{"body", "completed", "element", "index"}},
	// while: pullBool "condition"; fires body/completed.
	"core.flow.while@1": {inputs: []string{"condition"}, outputs: []string{"body", "completed"}},
	// delay: pullFloat "seconds" (exec extension op); fires "then".
	"core.flow.delay@1": {inputs: []string{"seconds"}, outputs: []string{"then"}},

	// --- io / variables (exec_interpreter.go) -------------------------
	// print: pullData "value"; fires "then".
	"core.print@1": {inputs: []string{"value"}, outputs: []string{"then"}},
	// variable.set: config "variable"; pullData "value"; fires "then".
	"core.variable.set@1": {inputs: []string{"value"}, outputs: []string{"then"}, config: []string{"variable"}},
	// output: pullData "value"; fires "then"; config "name" (the leaf).
	"core.output@1": {inputs: []string{"value"}, outputs: []string{"then"}, config: []string{"name"}},

	// --- async effects (exec_effects.go) ------------------------------
	//
	// http.request is now (ADR 010 §3.2) a real exec effect node: the seed
	// `core.http.request@1` declares exec pins `in`/`then`/`error` ON TOP
	// of its rich data surface, so `isExecNode` is true and execHTTPRequest
	// is reachable from an authored graph (the pre-ADR-010 unreachability
	// bug is closed). The runtime reads the DATA inputs
	// url/method/query/headers/body/timeout_ms, binds status/ok/body/headers
	// on `then`, and fires `error` on failure — all pinned below.
	//
	// source.read declares NO exec pins in the seed (pure dataflow — emits a
	// descriptor). Since ADR 012 Option B it is a PURE COMPUTE (compute_source.go),
	// not an exec effect: the compiler resolves its `source_id` config into a
	// compiler-injected `__resolved_source` key at compile, and the compute
	// returns that object as its single value (Option A). The runtime reads
	// only `__resolved_source` — a compiler-injected `__`-prefixed key,
	// deliberately NOT a seed config key, so it is intentionally absent from
	// runtimeContract (which pins only AUTHORED seed strings). The authored
	// seed config key is `source_id`, pinned below; the four output pins are
	// projected downstream by get-field (Option A), so the runtime hardcodes
	// no output-pin names — trivially a subset of the seed signature.
	"core.http.request@1": {inputs: []string{"url", "method", "query", "headers", "body", "timeout_ms"}, outputs: []string{"status", "ok", "body", "headers", "then", "error"}},
	"core.db.query@1":     {inputs: []string{"descriptor"}, outputs: []string{"rows", "count", "elapsed_ms", "error", "then"}, config: []string{"datasource"}},
	"core.source.read@1":  {config: []string{"source_id"}},

	// --- show event bridge (exec_show_emit.go, ADR 009 §3.6) ----------
	// show.emit: config "topic"; pullData "payload"; fires "then". A real
	// exec effect node — exec_in "in" / exec_out "then" in the seed; no
	// "error" pin (Blue#73 — delivery is construction-safe). The active-only
	// injection it performs writes __events.<topic> on show.Active(), an
	// INTERNAL runtime path (not a seed port), so only the authored
	// vocabulary (topic/payload/then) is pinned here.
	"core.show.emit@1": {inputs: []string{"payload"}, outputs: []string{"then"}, config: []string{"topic"}},

	// --- compute (compute_db.go / compute_pure.go config readers) -----
	// db.* descriptor builders read these config keys (compute_db.go).
	"core.db.from@1":   {config: []string{"table"}},
	"core.db.where@1":  {inputs: []string{"plan", "value"}, outputs: []string{"plan"}, config: []string{"column", "op"}},
	"core.db.join@1":   {inputs: []string{"plan"}, outputs: []string{"plan"}, config: []string{"table", "local_column", "foreign_column", "select"}},
	"core.db.select@1": {inputs: []string{"plan"}, outputs: []string{"plan"}, config: []string{"columns"}},
	"core.db.order@1":  {inputs: []string{"plan"}, outputs: []string{"plan"}, config: []string{"column", "direction"}},
	"core.db.limit@1":  {inputs: []string{"plan", "n"}, outputs: []string{"plan"}, config: []string{"n"}},
	// variable.get: config "variable" (compiler nodeLeafPath).
	"core.variable.get@1": {config: []string{"variable"}},
}

// TestExecPortParity_RuntimeStringsExistInSeed asserts every port/config
// string the Orion runtime hardcodes for a served node exists in that
// node's Blue seed signature. A drift fails LOUD and specific.
func TestExecPortParity_RuntimeStringsExistInSeed(t *testing.T) {
	sigs := conformance.Signatures()
	for id, rc := range runtimeContract {
		t.Run(id, func(t *testing.T) {
			sig, ok := sigs[id]
			if !ok {
				t.Fatalf("%s: runtime serves it but it has no seed signature in signatures.json", id)
			}
			assertSubset(t, id, "input", rc.inputs, sig.Inputs)
			assertSubset(t, id, "output", rc.outputs, sig.Outputs)
			assertSubset(t, id, "config", rc.config, sig.Config)
		})
	}
}

// TestExecPortParity_EverySeedExecPinHonoured asserts the dual direction:
// every EXEC pin the seed declares on a served exec node is one the
// runtime knows (no seed exec in/out pin silently ignored). Data pins are
// optional to read (an unwired data input is legal), so only exec pins —
// the control-flow contract — are required to be honoured. This is what
// would have caught the gate `toggle` pin being in the runtime but not the
// seed had it gone the other way (a seed pin the runtime drops).
func TestExecPortParity_EverySeedExecPinHonoured(t *testing.T) {
	// Exec pins per node, derived from the seed signature minus the data
	// pins. We can't tell exec from data in the name-only fixture, so we
	// pin the exec-pin sets the runtime must honour explicitly (reviewed
	// against stdlib_seeder.py). Each MUST be a subset of runtimeContract.
	seedExecPins := map[string]struct {
		in  []string
		out []string
	}{
		"core.flow.gate@1":     {in: []string{"enter", "open", "close", "toggle"}, out: []string{"then"}},
		"core.flow.branch@1":   {out: []string{"true", "false"}},
		"core.flow.sequence@1": {out: []string{"then_0", "then_1", "then_2"}},
		"core.flow.for-loop@1": {out: []string{"body", "completed"}},
		"core.flow.for-each@1": {out: []string{"body", "completed"}},
		"core.flow.while@1":    {out: []string{"body", "completed"}},
		"core.flow.delay@1":    {out: []string{"then"}},
		"core.print@1":         {out: []string{"then"}},
		"core.variable.set@1":  {out: []string{"then"}},
		"core.output@1":        {out: []string{"then"}},
		// http.request: exec_in "in", exec_out "then"/"error" (ADR 010 §3.2 —
		// a real exec effect node). source.read declares NO exec pins in the
		// seed (pure dataflow) — intentionally absent. db.query IS a real
		// exec effect node.
		"core.http.request@1": {out: []string{"then", "error"}},
		"core.db.query@1":     {out: []string{"then", "error"}},
		// show.emit: exec_in "in", exec_out "then", NO error pin (Blue#73 —
		// construction-safe delivery). The runtime honours `then` (the empty
		// outcome defaults to it); `in` is the generic entry pin the
		// interpreter routes, not a name the executor reads.
		"core.show.emit@1":               {out: []string{"then"}},
		"core.event.on-start@1":          {out: []string{"then"}},
		"core.event.on-tick@1":           {out: []string{"then"}},
		"core.event.on-event@1":          {out: []string{"then"}},
		"core.event.on-platform-event@1": {out: []string{"then"}},
		// on-call: exec out `then` (no exec in — it is an entrypoint).
		"core.operator.on-call@1": {out: []string{"then"}},
		// await-value: exec in `in`, exec out `then` (the suspend twin of
		// delay; `value` is a DATA out, bound on resume, not an exec pin).
		"core.operator.await-value@1": {out: []string{"then"}},
	}
	for id, pins := range seedExecPins {
		t.Run(id, func(t *testing.T) {
			rc, ok := runtimeContract[id]
			if !ok {
				t.Fatalf("%s: declared exec pins but absent from runtimeContract", id)
			}
			assertSubset(t, id, "seed-exec-in", pins.in, rc.inputs)
			assertSubset(t, id, "seed-exec-out", pins.out, rc.outputs)
		})
	}
}

// assertSubset fails if any string in `have` is not in `want`.
func assertSubset(t *testing.T, id, kind string, have, want []string) {
	t.Helper()
	set := map[string]struct{}{}
	for _, w := range want {
		set[w] = struct{}{}
	}
	missing := []string{}
	for _, h := range have {
		if _, ok := set[h]; !ok {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%s: runtime %s %v absent from seed signature %v", id, kind, missing, want)
	}
}
