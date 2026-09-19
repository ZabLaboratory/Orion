package compiler

import (
	"encoding/json"
	"fmt"
	"github.com/ZabLaboratory/Orion/internal/conformance"
)

// validateBlueprint checks every node's compute is in Blue's manifest
// and builds the DATA-layer graph nodes plus a defaults map seeded from
// declared inputs. EXEC-layer nodes (those in execSet — any node with an
// exec pin, ADR 006 §3.1) are skipped: they are routed to the blueprint's
// ExecProgram by partitionBlueprint, never recomputed as data nodes
// (closing the silent-skip hole, ADR 006 §1). Purity is NO LONGER a
// rejection gate (ADR 006 §3.2): an impure data compute is served, not
// refused — capability is total, proof is the validation gate's job.
func validateBlueprint(b *BlueprintGraph, manifest ComputeManifest, execSet map[string]struct{}) ([]GraphNode, map[string]json.RawMessage, []Diagnostic) {
	var diags []Diagnostic
	defaults := map[string]json.RawMessage{}
	nodes := make([]GraphNode, 0, len(b.Nodes))

	// Upstream ids and named inputs are built off the SAME edge walk so
	// they stay zipped 1:1 (issue #79): Upstream[i] == Inputs[i].From for
	// every i. Inputs carries the edge's to_port verbatim — the runtime
	// delivers each upstream value under that declared name.
	upstreams := make(map[string][]string)
	inputs := make(map[string][]GraphInput)
	for _, e := range b.Edges {
		upstreams[e.ToNode] = append(upstreams[e.ToNode], e.FromNode)
		// Carry from_port ONLY when the producer is an EXEC node: a counted
		// loop exposes several data-out pins (`index`/`element`) and the
		// consumer must name which one, so demandValue can resolve the
		// per-iteration env pin `<from>.<from_port>`. Ordinary data producers
		// have a single output pin — their from_port is decorative, and
		// emitting it would break the byte-identical-artefact invariant for
		// every pre-existing pure-dataflow scene (ADR 006 §3.1). So it stays
		// omitted there.
		gi := GraphInput{From: e.FromNode, Port: e.ToPort}
		if _, fromExec := execSet[e.FromNode]; fromExec {
			gi.FromPort = e.FromPort
		}
		inputs[e.ToNode] = append(inputs[e.ToNode], gi)
	}

	for _, n := range b.Nodes {
		// (a) Exec-layer nodes (ADR 006 §3.1) are routed to the ExecProgram
		// by partitionBlueprint; they are NOT data nodes and must not
		// appear in the GraphNode list. Skipping them here (and their
		// edges fall away in topologicalSort, which drops edges to
		// dropped nodes) closes the silent-skip hole. This check runs
		// FIRST so exec nodes bypass the manifest gate.
		if _, isExec := execSet[n.ID]; isExec {
			continue
		}

		// ADR 007 §3.3: the former structural guard (DB_NODE_OUTSIDE_QUERY)
		// is retired. The six core.db.* clause atomics are ordinary pure
		// computes (KindCompute, compute_db.go) composable in the main
		// graph; they flow through the manifest lookup below like any other
		// data node.

		// (c) Manifest validation: the compute must be known to Blue.
		// Purity is NO LONGER a rejection gate (ADR 006 §3.2): an impure
		// data compute is served, not refused.
		entry, found := manifest[n.Compute]
		if !found {
			diags = append(diags, Diagnostic{
				Code:     ErrUnknownComputeNode,
				Severity: "error",
				Message:  fmt.Sprintf("blueprint node %s references unknown compute %q", n.ID, n.Compute),
				Path:     n.ID,
			})
			continue
		}

		// Derive kind + leaf path from the REAL node body (config /
		// inputs / outputs), per ADR 004 §7.2. The leaf-path rule is
		// isolated in nodeLeafPath — it is the single seam Atlas flagged
		// for the residual question (the exact config key Prism's
		// blueprint editor authors for an output name). If that ever
		// diverges from the stdlib signature, this one function changes,
		// not the struct shape.
		//
		// Platform-event nodes (`quasar.<platform>.<event>@N`, ADR 003
		// §3.3.3 / issue #84) take the dedicated expansion instead: their
		// leaf is the GLOBAL Quasar-written address derived from the node
		// name + authored config.channel, byte-identical to Blue's
		// declared `signature.platform.leaf_path` and to Quasar's
		// `leaf_path(event)`.
		var path string
		if platform, event, isPlatform := platformNodeRef(n.Compute); isPlatform {
			leaf, pd := platformLeafPath(n, platform, event)
			if pd != nil {
				diags = append(diags, *pd)
				continue
			}
			path = leaf
		} else {
			path = nodeLeafPath(n)
		}

		kind := "computed"
		switch {
		case n.Compute == coreOutput:
			// An explicit output sink — the leaf the runtime writes to.
			kind = "output"
		case len(upstreams[n.ID]) == 0 && !isSourceCompute(n.Compute):
			// A leaf with no upstream: an adapter/operator input
			// (core.input@1) or a constant source (core.literal@1).
			//
			// EXCEPTION (the `from`-table regression): a CHAIN-HEAD compute
			// builder such as core.db.from@1 also has no upstream, yet it is
			// a registered runtime compute (KindCompute) that derives its
			// output ENTIRELY from config (`table`). Misclassifying it as
			// `input` made computeAt treat it as adapter-written (it never
			// ran dbFromFn) AND dropped its config below (config is carried
			// for `computed` nodes only) — so the descriptor reached
			// core.db.query@1 with an empty table → ZabRanking 422
			// `string_too_short` on body.table → 0 rows on air. Keeping it
			// `computed` runs the builder and carries its config.
			kind = "input"
		}

		// Seed graph.Defaults from constant sources and unwired ports.
		// core.literal@1's config.value is the constant; it seeds the
		// literal's own output leaf (replaces the old Args["default"]).
		if n.Compute == coreLiteral && path != "" {
			if v, ok := n.Config["value"]; ok {
				defaults[path] = v
			}
		}
		// Unwired input ports seed their declared fallback so a node
		// whose port has no inbound edge still has a value at cold start.
		for _, p := range n.Inputs {
			if p.Default == nil {
				continue
			}
			if _, wired := wiredPorts(n.ID, b.Edges)[p.Name]; wired {
				continue
			}
			defaults[n.ID+"."+p.Name] = p.Default
		}

		// Carry config to the runtime for COMPUTED nodes only (issue
		// #81): Blue's handlers receive (inputs, config) and the pure
		// data tranche needs it (get-field/set-field `path`, aggregate
		// `op`). input/output/literal configs are already lowered into
		// Path / Defaults above — carrying them again would only churn
		// the scene_version hash for nothing.
		var cfg map[string]json.RawMessage
		if kind == "computed" && len(n.Config) > 0 {
			cfg = n.Config
		}

		nodes = append(nodes, GraphNode{
			ID:        n.ID,
			Kind:      kind,
			Path:      path,
			Compute:   n.Compute,
			Upstream:  upstreams[n.ID],
			Inputs:    inputs[n.ID],
			IsPure:    entry.IsPure,
			IsBounded: entry.IsBounded,
			Config:    cfg,
		})
	}
	return nodes, defaults, diags
}

// isSourceCompute reports whether a node id is served by the runtime
// COMPUTE registry (conformance KindCompute) — a pure compute with a real
// runtime fn. Such a node must classify `computed` even with no upstream:
// it is a chain-head builder (core.db.from@1) that derives its output from
// config alone, NOT an adapter-written input. core.input@1 / core.literal@1
// are KindLeafBound (not KindCompute), so they correctly stay `input`.
// Mirrors the conformance matrix's single source of truth, the same table
// exec_partition.go's buildExecNode consults.
func isSourceCompute(compute string) bool {
	sn, ok := conformance.Classify(compute)
	return ok && sn.Kind == conformance.KindCompute
}

// coreSourceRead is the introspection compute reclassified by ADR 012
// (Option B). Its `source_id` config names a DECLARED ExternalAdapter; the
// compiler resolves it here and folds the descriptor into the node config.
const coreSourceRead = "core.source.read@1"

// resolvedSourceConfigKey is the reserved compiler-injected config key the
// resolved source descriptor is folded into (ADR 012 §1.2). It is mirrored
// verbatim by the runtime reader (runtime.resolvedSourceConfigKey,
// compute_source.go). Double-underscore = compiler-injected, never an
// authored key.
const resolvedSourceConfigKey = "__resolved_source"

// resolvedSource is the introspection projection the compiler folds into a
// source.read node's config (ADR 012 §1.2/§1.3). `name`/`kind` echo the
// adapter; `descriptor` is the structural projection of the adapter; the
// runtime returns this whole object as the node's single value (Option A),
// and a blueprint projects an individual pin downstream via get-field.
type resolvedSource struct {
	Name       string             `json:"name"`
	Kind       string             `json:"kind"`
	Descriptor resolvedDescriptor `json:"descriptor"`
}

type resolvedDescriptor struct {
	Label       string   `json:"label"`
	TargetPaths []string `json:"target_paths"`
	FrequencyHz *float64 `json:"frequency_hz"`
	Channel     *string  `json:"channel"`
}

// resolveSourceReads folds the pre-resolved descriptor into every
// `core.source.read@1` node's config and rejects undeclared sources (ADR
// 012 §1.2/§1.4). It mutates the node Config in place; a node was emitted as
// `computed` (isSourceCompute) so it already carries its config map. The
// reject is appended to d; the caller's HasErrors gate turns it into a
// POST /push failure.
func resolveSourceReads(nodes []GraphNode, adapters []ExternalAdapter, d *Diagnostics) {
	var byKey map[string]*ExternalAdapter
	for i := range nodes {
		n := &nodes[i]
		if n.Compute != coreSourceRead {
			continue
		}
		// Authored config key is `source_id` (stdlib_seeder.py).
		var sourceID string
		if raw, ok := n.Config["source_id"]; ok {
			_ = json.Unmarshal(raw, &sourceID)
		}
		if byKey == nil {
			byKey = make(map[string]*ExternalAdapter, len(adapters))
			for j := range adapters {
				byKey[adapters[j].Key] = &adapters[j]
			}
		}
		adapter, found := byKey[sourceID]
		if sourceID == "" || !found {
			d.Items = append(d.Items, Diagnostic{
				Code:     ErrSourceNotDeclared,
				Severity: "error",
				Message: fmt.Sprintf(
					"source.read node %s references undeclared source %q", n.ID, sourceID),
				Path: n.ID,
			})
			continue
		}
		var channel *string
		if adapter.Channel != "" {
			c := adapter.Channel
			channel = &c
		}
		targetPaths := adapter.TargetPaths
		if targetPaths == nil {
			targetPaths = []string{}
		}
		resolved := resolvedSource{
			Name: adapter.Key,
			Kind: adapter.Kind,
			Descriptor: resolvedDescriptor{
				Label:       adapter.Label,
				TargetPaths: targetPaths,
				FrequencyHz: adapter.FrequencyHz,
				Channel:     channel,
			},
		}
		raw, err := json.Marshal(resolved)
		if err != nil {
			// Marshal of a plain struct of JSON-safe fields cannot fail;
			// guard fail-closed rather than panic.
			d.Items = append(d.Items, Diagnostic{
				Code:     ErrSourceNotDeclared,
				Severity: "error",
				Message:  fmt.Sprintf("source.read node %s: descriptor marshal: %v", n.ID, err),
				Path:     n.ID,
			})
			continue
		}
		if n.Config == nil {
			n.Config = map[string]json.RawMessage{}
		}
		n.Config[resolvedSourceConfigKey] = raw
	}
}

// Stdlib node references whose body carries a state-leaf-bearing config
// (ADR 004 §7.2, source: Blue/src/blue/services/stdlib_seeder.py).
const (
	coreOutput       = "core.output@1"         // config.name → the leaf the runtime writes
	coreInput        = "core.input@1"          // config.name → the interface input name
	coreLiteral      = "core.literal@1"        // config.value → seeds graph.Defaults
	coreVariableGet  = "core.variable.get@1"   // config.variable → reads __vars.<key>.<variable>
	coreEventOnStart = "core.event.on-start@1" // exec entry that fires at scene load
)

// varsLeafPrefix is the namespace `variable.set`/`variable.get` share for
// blueprint-local state. The runtime's execVariableSet writes
// `__vars.<blueprint_key>.<name>` (exec_interpreter.go execVariableSet) — the
// key sits INSIDE the prefix, with the empty legacy key collapsing to the
// double-dot form `__vars..<name>`. variable.get must READ the byte-identical
// address, so nodeLeafPath emits exactly that empty-key form `__vars..<name>`
// (correct as-is for a single-blueprint push, key ""), and prefixGraphNodes
// rewrites the empty key segment to a real key for multi-blueprint scenes —
// never front-prefixing (`<key>.__vars.<name>`), which would NOT match set.
const varsLeafPrefix = "__vars."

// varsLeaf builds the empty-key leaf form a variable.get binds to:
// `__vars..<name>` — byte-identical to execVariableSet's write with an empty
// BlueprintKey. prefixGraphNodes substitutes the key for a keyed blueprint.
func varsLeaf(name string) string { return varsLeafPrefix + "." + name }

// foldDeclaredVariables seeds a blueprint's top-level `variables[].value`
// CONSTANTS into its compile defaults under the empty-key `__vars..<name>`
// address the inlined `core.variable.get@1` reads (Orion #192, ADR 016 RC-6).
//
// On the PUSH path the reference expander harvests an INLINED function's
// variables into BlueprintGraph.Defaults, but a reference-free top-level
// blueprint never runs the expander, so its OWN `variables[]` were dropped —
// `palette` stayed empty at runtime → `score-to-color` resolved null → the
// per-player `pl.*.color` leaves stuck at the COLD_COLOR placeholder (e2e #152).
// This is the exact logic the in-body simulate path (compile_exec_inbody.go)
// already applies; both call sites now share it so the two paths can never
// drift.
//
// INVARIANT (ADR 003/006 §reseed): only a variable carrying a `value` is a
// CONSTANT and gets folded. A value-less variable is pure mutable shared state
// (written by a `variable.set` before any read, reseeded from declared defaults
// on activation) — it MUST NOT be seeded here, so it is skipped. An empty-named
// variable is malformed and skipped. The leaf is emitted in the empty-key form;
// the caller's prefixDefaultLeaf substitutes the real blueprint key (no-op for
// the legacy "" key → byte-identical to a blueprint without declared constants).
func foldDeclaredVariables(dst map[string]json.RawMessage, vars []BlueprintVariable) {
	for _, v := range vars {
		if len(v.Value) == 0 || v.Name == "" {
			continue
		}
		dst[varsLeaf(v.Name)] = v.Value
	}
}

// nodeLeafPath returns the state leaf a blueprint node's result is
// written to, or "" for an intermediate compute whose outputs only feed
// downstream nodes via edges (core.math.*, core.compare.*, …).
//
// This is the leaf-path rule that replaces the phantom OutputAt
// (ADR 004 §7.2). It is deliberately the ONLY place the wire body is
// translated into a leaf address, so the residual question — the exact
// config key Prism's blueprint editor writes for an output's name — has
// a single seam to adjust if a real Prism-authored blueprint ever
// diverges from the stdlib signature.contract.
//
// Sink nodes (core.output@1 / core.input@1) name their leaf in
// config.name. A literal (core.literal@1) has no config.name; its output
// leaf is the node's own id (matching scene.go's upstreamPath fallback,
// which addresses an unnamed upstream node by its id). All other nodes
// return "" — their outputs are consumed off edges, never as leaves.
func nodeLeafPath(n BlueprintNode) string {
	switch n.Compute {
	case coreOutput, coreInput:
		if raw, ok := n.Config["name"]; ok {
			var name string
			if err := json.Unmarshal(raw, &name); err == nil && name != "" {
				return name
			}
		}
		return ""
	case coreLiteral:
		// A literal seeds Defaults at its own output leaf; the runtime
		// addresses an unnamed upstream by node id (scene.go:362-371).
		return n.ID
	case coreVariableGet:
		// variable.get is leaf-bound (conformance KindLeafBound): it has no
		// runtime executor, it READS the `__vars` leaf that variable.set
		// wrote. The pre-prefix form `__vars.<name>` becomes
		// `__vars.<key>.<name>` after prefixGraphNodes inserts the blueprint
		// key inside the prefix — byte-identical to execVariableSet's write
		// (`__vars.<BlueprintKey>.<name>`). Without this, the node fell into
		// the default ("" path) → classified input → demandValue read the
		// node id leaf (never written) → 0 → cross-tick reads froze (the
		// counter-stuck-at-1 bug observed on air).
		// Seed `core.variable.get@1` declares its config key as `variable`
		// (stdlib_seeder.py), the same key as `core.variable.set@1` — both
		// name the graph variable. Reading the seed's key keeps a set/get
		// pair pointed at the byte-identical `__vars.<key>.<variable>` leaf.
		if raw, ok := n.Config["variable"]; ok {
			var name string
			if err := json.Unmarshal(raw, &name); err == nil && name != "" {
				return varsLeaf(name)
			}
		}
		return ""
	default:
		return ""
	}
}
