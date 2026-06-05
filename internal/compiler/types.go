package compiler

import "encoding/json"

// PushEnvelope is the body of POST /api/v1/scenes/{id}/push (ADR 004
// § 7). One of (Definition, RollbackTo) is set; the API layer
// validates the mutual exclusion before constructing this struct.
type PushEnvelope struct {
	// Definition fields (regular push).
	CanvasVersion   string         `json:"canvas_version,omitempty"`
	BlueBlueprintID string         `json:"blue_blueprint_id,omitempty"`
	Components      []ComponentRef `json:"components,omitempty"`

	// LSMLBundleHash is the LSML content address Canvas/Prism computed
	// for this scene's authored bundle (the A0 store key, ADR 007 §C.4).
	// It is OPTIONAL and additive: an old Canvas omits it, in which case
	// Orion mints scene_version legacy-style exactly as before. When
	// present, Orion recompiles, emits its own LSML bundle, hashes it,
	// and — only on a byte-match — adopts this value as scene_version
	// (the identity collapse). A mismatch falls back to the legacy mint
	// plus an LSML_HASH_MISMATCH warning. The field is read only in
	// dual|lsdp mode; in bespoke mode it is ignored entirely.
	LSMLBundleHash string `json:"lsml_bundle_hash,omitempty"`

	// Rollback path: re-points latest_pushed_version at an existing
	// scene_version without recompilation.
	RollbackTo string `json:"rollback_to,omitempty"`
}

// ComponentRef names a pushed user-component version that the scene
// includes. The compiler fetches the matching artefact and inlines
// it into the scene tree.
type ComponentRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// IsRollback reports whether the envelope is a rollback request.
func (p PushEnvelope) IsRollback() bool { return p.RollbackTo != "" }

// CanvasLayout is the scene's authored tree from Canvas. The compiler
// walks it, resolves component refs, and emits the render bundle.
type CanvasLayout struct {
	Version string         `json:"version"`
	Root    LayoutNode     `json:"root"`
	Inputs  []OperatorInput `json:"operator_inputs,omitempty"`
}

// LayoutNode is one node in the layout tree. Either a Solar primitive
// (kind in primitive set) or a user-component reference (kind ==
// component_id, fetched via ComponentRef).
type LayoutNode struct {
	Kind       string                     `json:"kind"`
	ID         string                     `json:"id,omitempty"`
	Props      map[string]json.RawMessage `json:"props,omitempty"`
	Bindings   map[string]string          `json:"bindings,omitempty"`
	Transitions map[string]json.RawMessage `json:"transitions,omitempty"`
	Children   []LayoutNode               `json:"children,omitempty"`

	// component_id is set when Kind names a user component (the
	// authored layout uses the component's id directly as the kind
	// string; the compiler distinguishes by lookup against the
	// pushed components fetched via ComponentRef).
	ComponentArgs map[string]json.RawMessage `json:"component_args,omitempty"`
}

// OperatorInput is the declared operator surface for a path.
// Mirrors ADR 003 § 7.1.
type OperatorInput struct {
	Path        string          `json:"path"`
	Label       string          `json:"label"`
	Type        string          `json:"type"`
	Default     json.RawMessage `json:"default,omitempty"`
	Group       string          `json:"group,omitempty"`
	WritableBy  []string        `json:"writable_by,omitempty"`
	OptionsSrc  string          `json:"options_source,omitempty"`
	Min         *float64        `json:"min,omitempty"`
	Max         *float64        `json:"max,omitempty"`
	Step        *float64        `json:"step,omitempty"`
	MaxLength   *int            `json:"max_length,omitempty"`
	Regex       string          `json:"regex,omitempty"`
	EnumValues  []string        `json:"enum_values,omitempty"`
}

// ExternalAdapter is one declared external input source pulled out
// of the scene's bindings (HTTP poll, platform stream, scheduler tick).
type ExternalAdapter struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Kind        string   `json:"kind"` // http-poll | pg-listen | platform-stream | tick | …
	TargetPaths []string `json:"target_paths"`
	FrequencyHz *float64 `json:"frequency_hz,omitempty"`
	URL         string   `json:"url,omitempty"`
	Channel     string   `json:"channel,omitempty"` // pg-listen channel
}

// AssetRef points at a compiled asset.
type AssetRef struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Kind string `json:"kind"` // image | video | audio
}

// UserComponent is an authored composition fetched by the compiler
// for inlining. Cycle detection runs over the graph defined by
// LayoutNode.Kind references between components.
type UserComponent struct {
	ID         string          `json:"id"`
	Version    string          `json:"version"`
	Parameters []ComponentParam `json:"parameters"`
	Body       LayoutNode      `json:"body"`
	Inputs     []OperatorInput `json:"operator_inputs,omitempty"`
}

// ComponentParam declares an input name + (optional) default.
type ComponentParam struct {
	Name    string          `json:"name"`
	Default json.RawMessage `json:"default,omitempty"`
}

// BlueprintGraph is the logic-level DAG fetched from Blue. Each node
// names a compute (resolved via Blue's manifest) plus inputs/output.
type BlueprintGraph struct {
	ID    string          `json:"id"`
	Nodes []BlueprintNode `json:"nodes"`
	Edges []BlueprintEdge `json:"edges"`
}

// BlueprintNode is one node in the blueprint. Compute is the registry
// id ("core.add", "quasar.twitch.chat@1", …); the compiler probes
// Blue's manifest for is_pure / is_bounded.
type BlueprintNode struct {
	ID       string                     `json:"id"`
	Compute  string                     `json:"compute"`
	OutputAt string                     `json:"output_at,omitempty"` // dotted leaf path the compute writes to
	Args     map[string]json.RawMessage `json:"args,omitempty"`
}

// BlueprintEdge connects FromNode.OutputPort to ToNode.InputPort. v1
// stores the wire on snake_case fields per Blue's chantier convention.
type BlueprintEdge struct {
	FromNode string `json:"from_node"`
	FromPort string `json:"from_port"`
	ToNode   string `json:"to_node"`
	ToPort   string `json:"to_port"`
}

// ComputeManifestEntry mirrors what Blue's
// GET /blue/api/v1/_compute-manifest returns per compute id. The
// compiler queries this to enforce purity (criterion 18).
type ComputeManifestEntry struct {
	IsPure             bool     `json:"is_pure"`
	IsBounded          bool     `json:"is_bounded"`
	DeclaredInputs     []string `json:"declared_inputs,omitempty"`
	DeclaredOutputType string   `json:"declared_output_type,omitempty"`
	Version            string   `json:"version"`
}

// ComputeManifest is the manifest map: compute id → entry.
type ComputeManifest map[string]ComputeManifestEntry
