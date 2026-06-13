package compiler

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ZabLaboratory/Orion/internal/conformance"
)

// This file is the compiler half of the exec activation lift (ADR 006
// §3.1, issue #103 — the R9 partition). It routes a blueprint's nodes
// between the DATA layer (the topologically-sorted GraphNode list the
// reactive engine walks) and the EXEC layer (the ExecProgram the
// interpreter — issue #82 — runs). Before this file, every blueprint
// node became a GraphNode; pure-but-exec nodes (`branch`, `sequence`,
// loops — `is_pure:true` in Blue's manifest) fell through as data nodes
// with no compute and were silently skipped (ADR 006 §1, the hole this
// closes). The partition routes them to the exec program instead.
//
// DISCRIMINATOR (ADR 006 §3.1): a node belongs to the exec layer iff it
// carries at least one port with Kind=="exec" (BlueprintPort.Kind),
// seeded by Blue on every port. `is_pure` is scheduling metadata only —
// it never routes (it cannot: `branch` is pure yet exec, `variable.get`
// is pure and data).
//
// EMIT-BUT-NEVER-INSTALL (ADR 006 §3.7): this code only PRODUCES the
// artefact. No install path is touched; programs are emitted into
// graph.ExecPrograms and stay dormant until the activation wiring
// (issue #106) installs them. A scene with no exec node emits no
// program → byte-identical artefact to pre-lift (criterion #2).

// execPinKind is Blue's port discriminator value for an exec pin
// (BlueprintPort.Kind, stdlib_seeder). Any other value (typically
// "data", or empty) is a data pin.
const execPinKind = "exec"

// ErrExecOpUnmapped is the fail-loud structural diagnostic of the
// partition (ADR 006 §3.1): a node that carries exec pins (so it IS an
// exec node) but whose manifest id maps to no runtime exec op nor a
// known entrypoint. This is unreachable on a conformant build (ADR 003
// criterion 1 guarantees executor coverage, cross-checked by the
// conformance matrix). It exists so a coverage gap can never silently
// become accept-then-ignore — it is structural defence, NOT a rejection
// of a Blue capability.
const ErrExecOpUnmapped DiagnosticCode = "EXEC_OP_UNMAPPED"

// Exec entrypoint manifest ids → runtime entry Kind (ADR 006 §3.1).
// The runtime vocabulary ("on-start"/"on-tick"/"on-event") is owned by
// runtime.EntryOnStart/OnTick/OnEvent; the compiler cannot import the
// runtime package (it would cycle — runtime imports the compiler), so
// the three string literals are mirrored here. They are byte-pinned to
// the runtime by the round-trip test (crit #1), which decodes what this
// emits through runtime.ExecProgramsFromGraph.
var execEntryKind = map[string]string{
	"core.event.on-start@1":          "on-start",
	"core.event.on-tick@1":           "on-tick",
	"core.event.on-event@1":          "on-event",
	"core.event.on-platform-event@1": "on-platform-event",
}

// execEntryEventConfigKey is the single seam (mirroring nodeLeafPath's
// discipline) for the config key an `on-event` node carries its topic
// under: the entry listens to `__events.<event>`. If a real
// Prism-authored on-event node ever names this differently, this one
// constant changes, not the partition shape.
const execEntryEventConfigKey = "event_name"

// execMirror structs mirror the runtime exec program wire shape
// (runtime.ExecProgram/ExecEntry/ExecNode/ExecTarget/ExecDataInput)
// with byte-identical JSON tags. The compiler marshals these into the
// opaque json.RawMessage entries of graph.ExecPrograms; the runtime
// owns the schema and decodes them back (graph.ExecPrograms is
// json.RawMessage precisely so the compiler need not depend on the
// runtime type — same discipline the store applies to graph/bundle).
// The round-trip test asserts these tags match the runtime structs.

type execTarget struct {
	Node string `json:"node"`
	Port string `json:"port,omitempty"`
}

type execDataInput struct {
	Port     string `json:"port"`
	From     string `json:"from"`
	FromPort string `json:"from_port,omitempty"`
}

type execNode struct {
	ID     string                     `json:"id"`
	Op     string                     `json:"op"`
	Config map[string]json.RawMessage `json:"config,omitempty"`
	Data   []execDataInput            `json:"data,omitempty"`
	Next   map[string]execTarget      `json:"next,omitempty"`
}

type execEntry struct {
	Target execTarget `json:"target"`
	Kind   string     `json:"kind,omitempty"`
	Event  string     `json:"event,omitempty"`
	Node   string     `json:"node,omitempty"`
}

type execProgram struct {
	BlueprintKey string               `json:"blueprint_key"`
	Nodes        map[string]*execNode `json:"nodes"`
	Entrypoints  map[string]execEntry `json:"entrypoints"`
}

// isExecNode reports whether a blueprint node belongs to the exec layer
// — the partition discriminator. A node is exec iff ANY of its input or
// output ports carries Kind=="exec" (ADR 006 §3.1). Event nodes
// (on-start/on-tick/on-event) carry an exec OUT pin, so they classify
// exec here too; partitionBlueprint routes them to entrypoints.
func isExecNode(n BlueprintNode) bool {
	for _, p := range n.Inputs {
		if p.Kind == execPinKind {
			return true
		}
	}
	for _, p := range n.Outputs {
		if p.Kind == execPinKind {
			return true
		}
	}
	return false
}

// partitionBlueprint splits a blueprint's nodes into the exec set and
// the data set, returning the exec set as a node-id set plus the
// compiled execProgram (nil when the blueprint carries no exec node).
// The data tranche is handled by validateBlueprint's existing path; this
// function builds ONLY the exec program from the exec nodes + the exec
// and data edges that touch them.
//
// key is the scene-local blueprint key (ADR 001 §3.2); it becomes the
// program's BlueprintKey, namespacing `__vars.<key>.*` and the entry
// trigger index (`<key>/<entry>`). Node ids inside the program are NOT
// key-prefixed: the program is self-contained and the runtime resolves
// targets within it; key prefixing applies only to the merged data
// graph (prefixGraphNodes), whose addresses share one state namespace.
func partitionBlueprint(b *BlueprintGraph, key string) (execSet map[string]struct{}, prog *execProgram, diags []Diagnostic) {
	execSet = map[string]struct{}{}
	for _, n := range b.Nodes {
		if isExecNode(n) {
			execSet[n.ID] = struct{}{}
		}
	}
	if len(execSet) == 0 {
		return execSet, nil, nil
	}

	// Index input ports by node+port so an edge can tell whether the
	// port it lands on is exec (→ Next wiring) or data (→ ExecDataInput).
	inPortKind := map[string]string{}
	for _, n := range b.Nodes {
		for _, p := range n.Inputs {
			inPortKind[n.ID+"\x00"+p.Name] = p.Kind
		}
	}

	nodes := map[string]*execNode{}
	entries := map[string]execEntry{}

	for _, n := range b.Nodes {
		if _, isExec := execSet[n.ID]; !isExec {
			continue
		}
		if kind, isEntry := execEntryKind[n.Compute]; isEntry {
			diags = append(diags, buildExecEntry(n, kind, b.Edges, entries)...)
			continue
		}
		sn, opDiag := buildExecNode(n)
		if opDiag != nil {
			diags = append(diags, *opDiag)
			continue
		}
		nodes[n.ID] = sn
	}

	// Wire edges. An edge between two exec pins is control flow
	// (Next); an edge from a data producer into an exec node's data pin
	// is a pulled input (ExecDataInput). Edges OUT of an event node's
	// exec pin are already consumed as the entry Target above; an edge
	// landing on a data node is the data layer's concern (untouched).
	for _, e := range b.Edges {
		_, toExec := execSet[e.ToNode]
		if !toExec {
			continue // edge into a data node — handled by the data tranche
		}
		toKind := inPortKind[e.ToNode+"\x00"+e.ToPort]
		if toKind == execPinKind {
			// exec edge: wire the producer's exec OUT pin to this target.
			diags = append(diags, wireExecEdge(e, nodes, entries)...)
			continue
		}
		// data edge into an exec node: pull on demand. FromPort matters
		// when the producer is another exec node's data out (a loop's
		// index/element pin); carry it verbatim.
		dn := nodes[e.ToNode]
		if dn == nil {
			continue // target was an unmapped exec node (diagnosed above)
		}
		// From-id namespacing: a DATA-layer producer is addressed in the
		// runtime by its MERGED (key-prefixed) id — prefixGraphNodes
		// rewrites data node ids to `<key>.<id>`, and the interpreter's
		// demandValue resolves ExecDataInput.From against that prefixed
		// nodeIdx. So a data-producer From takes the same prefix. An
		// EXEC-producer reference (loop index/element pin) is resolved in
		// the task env by `<from>.<from_port>` WITHIN the program and is
		// never prefixed; the empty legacy key prefixes nothing (R4).
		from := e.FromNode
		if _, fromExec := execSet[e.FromNode]; !fromExec {
			from = prefixLeaf(key, from)
		}
		dn.Data = append(dn.Data, execDataInput{
			Port:     e.ToPort,
			From:     from,
			FromPort: e.FromPort,
		})
	}

	// Determinism: ExecDataInput lists are appended in edge order; sort
	// them by (Port, From, FromPort) so the artefact never depends on
	// the authored edge order (criterion #2).
	for _, sn := range nodes {
		sort.Slice(sn.Data, func(i, j int) bool {
			a, b := sn.Data[i], sn.Data[j]
			if a.Port != b.Port {
				return a.Port < b.Port
			}
			if a.From != b.From {
				return a.From < b.From
			}
			return a.FromPort < b.FromPort
		})
	}

	prog = &execProgram{
		BlueprintKey: key,
		Nodes:        nodes,
		Entrypoints:  entries,
	}
	return execSet, prog, diags
}

// buildExecNode maps one exec-pin node onto an execNode: its op is
// resolved from the manifest id through the conformance table (the
// single source of truth, CI-cross-checked against runtime.ExecOps).
// Config is carried verbatim (variable.set's `name`, delay's duration,
// branch's pins). A manifest-known exec node with no runtime op mapping
// → fail-loud EXEC_OP_UNMAPPED (ADR 006 §3.1).
//
// Unwired DATA-input defaults are folded into Config under the port name
// (#146 follow-up). Blue's seed declares a counted loop's control inputs
// as DATA pins with a default — `for-loop`'s `first`/`last`
// (_data_in(..., default=0)) and `for-each`'s `items` — never as config
// keys (signature.config == []). When the author types an inline literal
// instead of wiring an edge, that value lives on the input port's
// `default`, exactly as the data layer reads it (buildGraphNodes seeds
// graph.Defaults from `p.Default`). The interpreter resolves an exec
// node's data input by checking its wired producers (ExecDataInput)
// first, then `node.Config[port]` (pullData's fallback). Carrying ONLY
// n.Config dropped those defaults, so a literal-bounded `for-loop`
// reached the runtime with no `first`/`last` and `pullInt` fell to its
// def (-1 for `last`) → `0 <= -1` false → ZERO iterations, body never
// pushed. `while` was immune because its `condition` is always a wired
// comparison (no sensible literal), so it never depended on a port
// default — which is exactly why counted loops failed live while `while`
// worked. A WIRED edge still wins: it becomes an ExecDataInput, which
// pullData consults before the config fallback.
func buildExecNode(n BlueprintNode) (*execNode, *Diagnostic) {
	sn, ok := conformance.Classify(n.Compute)
	if !ok || sn.Kind != conformance.KindExecOp || sn.Op == "" {
		return nil, &Diagnostic{
			Code:     ErrExecOpUnmapped,
			Severity: "error",
			Message: fmt.Sprintf("exec node %s (%q) carries exec pins but maps to no runtime exec op",
				n.ID, n.Compute),
			Path: n.ID,
		}
	}
	cfg := foldInputDefaults(n)
	return &execNode{
		ID:     n.ID,
		Op:     sn.Op,
		Config: cfg,
	}, nil
}

// foldInputDefaults returns n.Config extended with each DATA input port's
// `default` under the port name, so the interpreter's config fallback
// (pullData) finds an inline-literal control value the author typed
// rather than wired. An author-supplied config key wins over a port
// default (config is the more specific authoring intent), and exec pins
// are skipped (they carry no data value). Returns nil when nothing is
// carried, preserving the byte-identical artefact for nodes with no
// config and no defaulted inputs (criterion #2).
func foldInputDefaults(n BlueprintNode) map[string]json.RawMessage {
	var cfg map[string]json.RawMessage
	ensure := func() {
		if cfg == nil {
			cfg = make(map[string]json.RawMessage, len(n.Config)+len(n.Inputs))
			for k, v := range n.Config {
				cfg[k] = v
			}
		}
	}
	if len(n.Config) > 0 {
		ensure()
	}
	for _, p := range n.Inputs {
		if p.Kind == execPinKind || len(p.Default) == 0 {
			continue
		}
		if _, exists := n.Config[p.Name]; exists {
			continue // author config is the more specific intent
		}
		ensure()
		cfg[p.Name] = p.Default
	}
	return cfg
}

// buildExecEntry compiles an event node into one ExecEntry per wired
// exec OUT edge (Blue validates an event node's exec out single-wired,
// but the partition tolerates zero or one). The entry is keyed by the
// event node's id so InstallExec's trigger index is deterministic; on
// activation the runtime fires it. on-event reads its `__events.<event>`
// topic from config.
func buildExecEntry(n BlueprintNode, kind string, edges []BlueprintEdge, entries map[string]execEntry) (diags []Diagnostic) {
	entry := execEntry{Kind: kind, Node: n.ID}
	if kind == "on-event" {
		if raw, ok := n.Config[execEntryEventConfigKey]; ok {
			var ev string
			if err := json.Unmarshal(raw, &ev); err == nil {
				entry.Event = ev
			}
		}
		if entry.Event == "" {
			diags = append(diags, Diagnostic{
				Code:     ErrExecOpUnmapped,
				Severity: "error",
				Message: fmt.Sprintf("on-event node %s declares no config.%s — cannot bind its __events topic",
					n.ID, execEntryEventConfigKey),
				Path: n.ID,
			})
			return diags
		}
	}
	if kind == "on-platform-event" {
		// The platform-event entry observes the canonical platform leaf
		// `__inputs.platform.<platform>.<channel>.last_<event_type>` (ADR
		// 013 §3) — NOT an `__events.` topic. Its config carries
		// platform/channel/event_type; the same casefold-then-validate
		// channel discipline as quasar.* nodes is reused (same charset,
		// same PLATFORM_CHANNEL_INVALID diagnostic). The leaf is also what
		// platformStreamBindings accepts, via collectExecEntryLeaves.
		leaf, pd := platformEventEntryLeaf(n)
		if pd != nil {
			diags = append(diags, *pd)
			return diags
		}
		entry.Event = leaf
	}
	// Find the single exec OUT edge to set Target. An entry with no
	// wired exec body fires a no-op task — structurally valid (the
	// authoring gap is the author's, not a capability rejection). Only
	// edges leaving an EXEC out pin set the Target: a data edge off the
	// entry (e.g. on-event `payload` → a get-field's data input) drives
	// pure dataflow reactivity, not an exec dispatch, and must never be
	// mistaken for the exec body — otherwise the runtime walks into a
	// data node and logs "unknown node id" (the exec node table holds
	// exec nodes only).
	execOutPins := make(map[string]struct{})
	for _, p := range n.Outputs {
		if p.Kind == execPinKind {
			execOutPins[p.Name] = struct{}{}
		}
	}
	for _, e := range edges {
		if e.FromNode != n.ID {
			continue
		}
		if _, isExecOut := execOutPins[e.FromPort]; !isExecOut {
			continue
		}
		entry.Target = execTarget{Node: e.ToNode, Port: e.ToPort}
		break
	}
	entries[n.ID] = entry
	return diags
}

// wireExecEdge sets Next[<from_port>] = {to_node, to_port} on the
// producer exec node. The from_port is the OUT pin name ("then",
// "true"/"false", "body"/"completed", "then_0".., "exit"…); Blue
// validates exec out-pins single-wired so the last writer wins
// deterministically (there is at most one). An exec edge whose producer
// is an event node is already the entry Target — skipped here (event
// nodes are not in `nodes`).
func wireExecEdge(e BlueprintEdge, nodes map[string]*execNode, entries map[string]execEntry) (diags []Diagnostic) {
	if _, isEntry := entries[e.FromNode]; isEntry {
		return nil // producer is an event node — wired as the entry Target
	}
	from := nodes[e.FromNode]
	if from == nil {
		return nil // producer was an unmapped exec node (diagnosed)
	}
	if from.Next == nil {
		from.Next = map[string]execTarget{}
	}
	from.Next[e.FromPort] = execTarget{Node: e.ToNode, Port: e.ToPort}
	return nil
}

// marshalExecProgram serializes one program into the opaque raw-JSON
// form graph.ExecPrograms carries. json.Marshal orders map keys
// (Nodes, Entrypoints, Config, Next) lexicographically, so the bytes
// are deterministic regardless of build-time map iteration order
// (criterion #2). Returns nil for a nil program.
func marshalExecProgram(p *execProgram) (json.RawMessage, error) {
	if p == nil {
		return nil, nil
	}
	return json.Marshal(p)
}
