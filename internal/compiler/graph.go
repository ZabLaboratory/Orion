package compiler

import "encoding/json"

// Graph is the runtime-facing artefact. Every consumer in the runtime
// reads it: the reactive engine for dirty propagation, the adapter
// inbox for path-to-scenes routing, the WS server for snapshot seeding.
//
// Field naming is wire-stable: this struct is persisted as JSONB and
// read back by future Orion versions.
type Graph struct {
	SceneID      string `json:"scene_id"`
	SceneVersion string `json:"scene_version"`

	// Nodes are stored in topological order. The runtime walks them
	// in this order during a recompute pass; dependencies always
	// precede dependents.
	Nodes []GraphNode `json:"nodes"`

	// Bindings declares external input sources (pollers, listens,
	// platform streams). Orion reads this at scene load to start the
	// adapter goroutines.
	Bindings []ExternalAdapter `json:"bindings"`

	// Defaults map every leaf path the runtime must seed at scene
	// activation / restart (live state is never persisted, so the
	// runtime reseeds from this on every cold start).
	Defaults map[string]json.RawMessage `json:"defaults"`

	// OperatorInputs is the canonical list every operator surface
	// (Solar overlay, Prism panel, Companion, mPrism) reads from.
	// Same shape as the bundle's; persisted on the graph too so
	// adapters that don't load Solar can fetch it directly.
	OperatorInputs []OperatorInput `json:"operator_inputs"`
}

// GraphNode is one node in the topologically-sorted DAG.
type GraphNode struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"` // input | computed | output
	Path     string   `json:"path,omitempty"`
	Compute  string   `json:"compute,omitempty"`
	Upstream []string `json:"upstream,omitempty"`

	// IsPure / IsBounded are mirrored from Blue's manifest at compile
	// time so the runtime can trust them without re-querying Blue.
	IsPure    bool `json:"is_pure,omitempty"`
	IsBounded bool `json:"is_bounded,omitempty"`
}

// RenderBundle is the Solar-facing artefact. Same shape as ADR 003 § 3.
type RenderBundle struct {
	SceneVersion     string            `json:"scene_version"`
	Root             LayoutNode        `json:"root"`
	OperatorInputs   []OperatorInput   `json:"operator_inputs"`
	ExternalAdapters []ExternalAdapter `json:"external_adapters"`
	Assets           []AssetRef        `json:"assets,omitempty"`
}
