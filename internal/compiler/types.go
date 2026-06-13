package compiler

import "encoding/json"

// PushEnvelope is the body of POST /api/v1/scenes/{id}/push (ADR 004
// § 7). One of (Definition, RollbackTo) is set; the API layer
// validates the mutual exclusion before constructing this struct.
type PushEnvelope struct {
	// Definition fields (regular push).
	CanvasVersion string `json:"canvas_version,omitempty"`

	// DEPRECATED but ACCEPTED (ADR 001 §3.1): the legacy single blueprint.
	// When set and Blueprints is empty, the API layer (scenes_push.go)
	// normalises it into a one-element Blueprints list with key "" (the
	// default/anonymous blueprint key) whose prefix is empty → leaf paths
	// stay byte-identical to the pre-001 single-blueprint behaviour. Emitted
	// by Prism/Canvas before they adopt `blueprints`. NEVER set together with
	// Blueprints — both present → 400 ENVELOPE_BLUEPRINT_CONFLICT.
	BlueBlueprintID string `json:"blue_blueprint_id,omitempty"`

	// Blueprints is the N distinct Blue blueprints this scene binds (ADR 001
	// §3.1). Each entry pairs a Blue blueprint id with a scene-local Key that
	// components reference (via LayoutNode.Bindings, leading dotted segment)
	// to declare which blueprint they consume (§3.3). Order is normalised
	// (sort by Key) before hashing so scene_version is stable regardless of
	// the authored order (§3.5). Mutually exclusive with BlueBlueprintID.
	Blueprints []BlueprintRef `json:"blueprints,omitempty"`

	Components []ComponentRef `json:"components,omitempty"`

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

// BlueprintRef pairs a Blue blueprint id with the scene-local key that
// components use to bind to it (ADR 001 §3.1). Key is unique within an
// envelope; the compiler prefixes every leaf path the blueprint
// contributes with "<Key>." (§3.3). The legacy length-1 list uses Key
// "" (empty prefix → byte-identical leaf paths to the pre-001 single
// blueprint). ID is the FetchBlueprint argument.
type BlueprintRef struct {
	Key string `json:"key"`
	ID  string `json:"id"`
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
	Version string          `json:"version"`
	Root    LayoutNode      `json:"root"`
	Inputs  []OperatorInput `json:"operator_inputs,omitempty"`

	// Animations is the inlined Animation Asset catalogue (ADR 011 §3.1 /
	// I1): `{animation_id: {target, keyframes{key?, duration_ms, easing,
	// steps[]}}}`, validated authoring-side by ZabCanvas
	// (schemas/animation_asset.py) and forwarded verbatim onto the served
	// CanvasLayout by the layout adapter. The compiler resolves
	// `animation.play.animation_id` against it at lowering
	// (lower_animation.go), never on the wire (§3.2/§3.4). Opaque
	// json.RawMessage: Orion is a transport for the keyframe shape (single
	// source of truth = the authoring schema + the Solar oracle), it binds
	// the `key` and frames it, it does not re-interpret the steps. Optional
	// + additive: a layout without animations leaves it nil → every
	// `animation` element falls through inert.
	Animations json.RawMessage `json:"animations,omitempty"`
}

// LayoutNode is one node in the layout tree. Either a Solar primitive
// (kind in primitive set) or a user-component reference (kind ==
// component_id, fetched via ComponentRef).
type LayoutNode struct {
	Kind        string                     `json:"kind"`
	ID          string                     `json:"id,omitempty"`
	Props       map[string]json.RawMessage `json:"props,omitempty"`
	Bindings    map[string]string          `json:"bindings,omitempty"`
	Transitions map[string]json.RawMessage `json:"transitions,omitempty"`
	Children    []LayoutNode               `json:"children,omitempty"`

	// component_id is set when Kind names a user component (the
	// authored layout uses the component's id directly as the kind
	// string; the compiler distinguishes by lookup against the
	// pushed components fetched via ComponentRef).
	ComponentArgs map[string]json.RawMessage `json:"component_args,omitempty"`

	// Keyframes carries the runtime `RenderNode.keyframes` block (LSML 1.1
	// §6.6: {key, duration_ms, easing, steps[]}) the Lumencast runtime's
	// KeyframePlayer replays on every delta at `key`. It is the served-bundle
	// form of a leaf-driven multi-step animation, produced by Orion's
	// lowering — see lowerWipeCover (ADR 003 Amendment 5 §A5.3, the chosen
	// authoring maillon "(B)"). The M10 `wipe-cover` authoring element lowers
	// to a `frame` node carrying this block.
	//
	// It is `omitempty` and ADDITIVE: every existing producer/consumer that
	// never sets it round-trips byte-identically (no Unmarshal drop, the field
	// simply stays nil). It rides ONLY on the lowered render bundle (`Root`):
	// it is NEVER authored on the pre-lowering tree, so AuthoringRoot never
	// carries it and EmitLSML (which reads Kind/ID/Props/Bindings/Transitions/
	// Children, not Keyframes — emit_lsml.go::lsmlNode) is unaffected. The C4
	// LSML content-hash (scenes_push.go, adopt-on-verify) therefore stays
	// stable: render-bundle-only, no LSML-emit change (SPIKE-LSML-HASH, A5.5).
	//
	// Carried as opaque json.RawMessage: Orion is a transport for the keyframe
	// shape, whose single source of truth is Solar's buildWipeCoverNode (the
	// parity oracle, A5.3). Orion builds it byte-for-byte in lowerWipeCover but
	// does not re-interpret it downstream.
	Keyframes json.RawMessage `json:"keyframes,omitempty"`

	// AnimateInitial carries the runtime `RenderNode.animate_initial` field
	// (LSML 1.1 §6: the `animate.from` mount-play state) — a FLAT framer-motion
	// map (`opacity`/`scale`/`rotate`/`x`/`y`) the Lumencast runtime (≥0.3.0)
	// passes verbatim as framer `initial={...}` so the node mounts in the
	// from-state and ramps to its target (mount-play). The runtime reads ONLY
	// this flat top-level field, never `transitions.from` — the schema mismatch
	// that blocked the M10 ramp (Pulsar runbook
	// m10-animate-initial-contract-hole, PR Pulsar#98).
	//
	// Produced by lowerAnimateInitial (the Go mirror of
	// @lumencast/compiler@0.3.0 lowerAnimateState, compile.ts:240-255) from the
	// `from` entry the authored `animate` directive carries inside Transitions.
	// `transitions` itself is left untouched (the runtime reads its timing).
	//
	// Same additive discipline as Keyframes: `omitempty`, never set when no
	// `from` is authored (rétro-compat: prior no-mount-play behaviour holds),
	// no Unmarshal drop. It rides ONLY on the lowered render bundle (`Root`):
	// AuthoringRoot never carries it and EmitLSML does not read it
	// (emit_lsml.go::lsmlNode), so the C4 LSML content-hash (scenes_push.go,
	// adopt-on-verify) is unperturbed — render-bundle-only.
	AnimateInitial json.RawMessage `json:"animate_initial,omitempty"`
}

// OperatorInput is the declared operator surface for a path.
// Mirrors ADR 003 § 7.1.
type OperatorInput struct {
	Path       string          `json:"path"`
	Label      string          `json:"label"`
	Type       string          `json:"type"`
	Default    json.RawMessage `json:"default,omitempty"`
	Group      string          `json:"group,omitempty"`
	WritableBy []string        `json:"writable_by,omitempty"`
	OptionsSrc string          `json:"options_source,omitempty"`
	Min        *float64        `json:"min,omitempty"`
	Max        *float64        `json:"max,omitempty"`
	Step       *float64        `json:"step,omitempty"`
	MaxLength  *int            `json:"max_length,omitempty"`
	Regex      string          `json:"regex,omitempty"`
	EnumValues []string        `json:"enum_values,omitempty"`
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
	ID         string           `json:"id"`
	Version    string           `json:"version"`
	Parameters []ComponentParam `json:"parameters"`
	Body       LayoutNode       `json:"body"`
	Inputs     []OperatorInput  `json:"operator_inputs,omitempty"`
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
// id ("core.math.add@1", "quasar.twitch.chat@1", …); the compiler probes
// Blue's manifest for is_pure / is_bounded.
//
// The wire field is `definition` — the qualified reference
// `namespace.name@version` — not `compute`. That is what every producer
// emits: Blue (graph.py:40, Node.definition) and Prism
// (src/renderer/src/lib/blue-types.ts:61, Node.definition). The Go field
// keeps the name Compute because that is the semantic role the compiler
// gives it (manifest[n.Compute] lookup); only the JSON tag binds to the
// canonical `definition` wire field. Tagging it `compute` — a field no
// producer emits — left Compute == "" on every real push, which made
// validateBlueprint look up manifest[""] and emit a spurious
// UNKNOWN_COMPUTE_NODE (issue #32, ADR 004 §7.1).
//
// Body shape (ADR 004 §7.2, issue #35). The earlier `OutputAt`
// (`json:"output_at"`) and `Args` (`json:"args"`) tags named fields no
// producer emits, so every node deserialised all-zero and the runtime
// graph was built blind (kind="input", path="", no default) without
// raising a diagnostic — a silently wrong push. The real node body, as
// shaped by Prism (src/renderer/src/lib/blue-types.ts:58-68) and stored
// by Blue (Blue/src/blue/schemas/graph.py:29-45), carries three fields:
//
//   - config: static config params keyed by the node's signature.config
//     names (Blue/src/blue/schemas/node_definition.py). Per-node meaning;
//     the leaf-path derivation reads config.name on core.output@1 /
//     core.input@1 and config.value on core.literal@1
//     (stdlib_seeder.py:648-701).
//   - inputs: typed input ports. Wiring lives on edges (to_node/to_port),
//     NOT here; inputs[].default is the unwired-port fallback.
//   - outputs: typed output ports. The compute writes its result(s) here;
//     edges (from_node/from_port) consume them.
type BlueprintNode struct {
	ID      string                     `json:"id"`
	Compute string                     `json:"definition"`
	Config  map[string]json.RawMessage `json:"config,omitempty"`
	Inputs  []BlueprintPort            `json:"inputs,omitempty"`
	Outputs []BlueprintPort            `json:"outputs,omitempty"`
}

// BlueprintPort mirrors Blue's Port schema
// (Blue/src/blue/schemas/graph.py:17-27): a typed input or output on a
// node. Field names are snake_case on the wire — both producer (Prism)
// and store (Blue) are owned by us, so no camelCase alias dance. `Kind`
// is the seeder's `data` | `exec` discriminator (stdlib_seeder.py:51,55,
// 68,75); it is `extra` on Blue's Port (ConfigDict(extra="allow")) but
// present on every seeded port, so Orion carries it. `Default` is the
// unwired-port fallback the runtime seeds into graph.Defaults.
type BlueprintPort struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Kind     string          `json:"kind,omitempty"`
	Required bool            `json:"required,omitempty"`
	Default  json.RawMessage `json:"default,omitempty"`
}

// BlueprintEdge connects FromNode.OutputPort to ToNode.InputPort. v1
// stores the wire on snake_case fields per Blue's chantier convention.
type BlueprintEdge struct {
	FromNode string `json:"from_node"`
	FromPort string `json:"from_port"`
	ToNode   string `json:"to_node"`
	ToPort   string `json:"to_port"`
}

// ComputeManifestEntry is Orion's internal view of one compute the
// compiler can resolve. The compiler queries this by compute id to
// enforce purity (criterion 18). It is BUILT from Blue's wire DTO
// (blueManifestEntry) — it is not the wire shape itself.
type ComputeManifestEntry struct {
	IsPure             bool     `json:"is_pure"`
	IsBounded          bool     `json:"is_bounded"`
	DeclaredInputs     []string `json:"declared_inputs,omitempty"`
	DeclaredOutputType string   `json:"declared_output_type,omitempty"`
	Version            string   `json:"version"`
}

// ComputeManifest is the manifest map: compute id → entry. The key is
// Blue's `node_id`, i.e. the qualified reference `namespace.name@version`
// (e.g. "core.math.add@1", "quasar.twitch.chat@1"). This is the SAME
// string a blueprint graph node carries in its `definition` field, so
// validateBlueprint can look an entry up by the node's compute ref.
//
// Source of truth for the key shape:
//   - Blue manifest:  Blue/src/blue/services/compute_manifest.py:126
//     node_id = f"{namespace}.{name}@{version}"
//   - Blue blueprint: Blue/src/blue/schemas/graph.py:32,40
//     Node.definition = "the qualified reference (namespace.name@version)"
type ComputeManifest map[string]ComputeManifestEntry

// blueManifestEntry mirrors Blue's wire DTO ManifestEntryDTO
// (Blue/src/blue/routes/compute_manifest.py:32-42). Field types match
// Blue verbatim: version is an INT, declared_inputs is a list of dicts,
// declared_output_type may be a string, a list of strings, or null.
// Orion adapts these into ComputeManifestEntry rather than decoding
// straight into it — decoding straight in is what broke every push
// (issue #30, found by the live E2E on 2026-06-05).
type blueManifestEntry struct {
	NodeID             string           `json:"node_id"`
	Namespace          string           `json:"namespace"`
	Name               string           `json:"name"`
	Version            int              `json:"version"`
	IsPure             bool             `json:"is_pure"`
	IsBounded          bool             `json:"is_bounded"`
	DeclaredInputs     []map[string]any `json:"declared_inputs"`
	DeclaredOutputType json.RawMessage  `json:"declared_output_type"`
	Source             string           `json:"source"`
	Platform           map[string]any   `json:"platform"`
}

// blueManifestResponse is Blue's envelope: {"entries":[...],"count":N}
// (Blue/src/blue/routes/compute_manifest.py:45-47).
type blueManifestResponse struct {
	Entries []blueManifestEntry `json:"entries"`
	Count   int                 `json:"count"`
}
