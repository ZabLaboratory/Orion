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

	// ExecPrograms carries the compiled exec layer — one entry per
	// blueprint (ADR 003 §3.1, issue #87). Each entry is an opaque
	// runtime.ExecProgram serialized as JSON: the compiler does NOT
	// know its schema (the runtime owns it), so it travels as raw bytes,
	// exactly like the store treats graph/bundle. The field is
	// omitempty: the compiler PARTITION that emits programs is a future
	// issue (R9 — exec stays dormant until then), so today's artefacts
	// carry nothing here and stay byte-identical. The validation harness
	// (#87) reads this set; an empty set means a pure-dataflow scene,
	// which validates trivially.
	ExecPrograms []json.RawMessage `json:"exec_programs,omitempty"`
}

// GraphNode is one node in the topologically-sorted DAG.
type GraphNode struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"` // input | computed | output
	Path     string   `json:"path,omitempty"`
	Compute  string   `json:"compute,omitempty"`
	Upstream []string `json:"upstream,omitempty"`

	// Inputs carries the NAMED wiring (ADR 003 §3.1.1, issue #79):
	// one entry per inbound data edge, pairing the upstream node id
	// with the blueprint edge's declared `to_port` name. The runtime's
	// gatherInputs delivers each upstream value under exactly that
	// name, so wiring is edge-order-independent. Entries are zipped
	// 1:1 with Upstream (same edges, same order). Pre-#79 persisted
	// artefacts lack the field — the runtime falls back to positional
	// `a..d` for them until their next push recompiles.
	Inputs []GraphInput `json:"inputs,omitempty"`

	// IsPure / IsBounded are mirrored from Blue's manifest at compile
	// time so the runtime can trust them without re-querying Blue.
	IsPure    bool `json:"is_pure,omitempty"`
	IsBounded bool `json:"is_bounded,omitempty"`

	// Config carries the blueprint node's config object verbatim into
	// the artefact (issue #81). Config-bearing pure computes read it at
	// execution time — `core.data.get-field`/`set-field` (`path`),
	// `core.data.aggregate` (`op`) — mirroring Blue's
	// handler(inputs, config, state) contract (executor.py). Only
	// COMPUTED nodes carry it: input/output/literal configs are already
	// lowered into Path / Defaults at compile. omitempty keeps pre-#81
	// artefacts and config-less nodes byte-identical.
	Config map[string]json.RawMessage `json:"config,omitempty"`
}

// GraphInput is one inbound data edge on a GraphNode: the upstream
// node id plus the destination port name (the blueprint edge's
// `to_port`, carried verbatim into the artefact — ADR 003 §3.1.1).
//
// FromPort carries the PRODUCER's out-pin name (the edge's `from_port`)
// when it disambiguates a multi-output producer. It matters when the
// producer is an EXEC node exposing several data-out pins — a counted
// loop's `index`/`element` (exec_interpreter.go binds them in the task
// env under `<node>.<from_port>`). For an ordinary single-output data
// producer it is empty (omitted), and demandValue resolves the producer
// by node id alone — byte-identical to pre-existing artefacts. Without
// it, an `add` fed by `for-loop.index` reached the runtime as
// `{from:"loop"}` with no pin, demandValue could not find
// `t.env["loop.index"]`, and the index read as 0 → the loop body summed
// nothing (the second half of the counted-loop iteration bug).
type GraphInput struct {
	From     string `json:"from"`
	Port     string `json:"port"`
	FromPort string `json:"from_port,omitempty"`
}

// RenderBundle is the Solar-facing artefact. Same shape as ADR 003 § 3.
type RenderBundle struct {
	SceneVersion     string            `json:"scene_version"`
	Root             LayoutNode        `json:"root"`
	OperatorInputs   []OperatorInput   `json:"operator_inputs"`
	ExternalAdapters []ExternalAdapter `json:"external_adapters"`
	Profiles         []string          `json:"profiles,omitempty"`
	Assets           []AssetRef        `json:"asset_refs,omitempty"`
	// Defaults are the static literal seeds emitted by CompileStaticLSML.
	// Keeping them in the immutable Solar bundle lets a validated runtime
	// capsule carry both the render tree and the initial snapshot.
	Defaults map[string]json.RawMessage `json:"defaults,omitempty"`

	// AuthoringRoot is the pre-lowering tree in the AUTHORING vocab
	// (`style.fontSize`/`color`, `size.{w,h}`, `geometry`, `cornerRadius`,
	// nested `stroke`) — i.e. `Root` BEFORE lowerRenderTree flattened it to
	// the flat render vocab Solar reads. It is `json:"-"`: it never travels
	// on the wire and is NOT part of the persisted bundle JSON nor the
	// scene_version hash, so the served RenderBundle (and the Solar render
	// fidelity proven in #41) is byte-identical with or without this field.
	//
	// It exists for one consumer: EmitLSML (ADR 007 §9.6 / §C.1). The LSML
	// 1.1 bundle MUST carry the authoring vocab — both because LSML is an
	// authoring-vocab format (§9.6) and because C4 adopt-on-verify hashes
	// the authoring tree on the Prism side (`sceneToLsml`); emitting from
	// the lowered `Root` produced a permanent LSML_HASH_MISMATCH (the bug
	// Vigil found on PR #42). The compile tail fills this with `expanded`
	// before lowering `Root`; the two trees are distinct objects (lowering
	// returns a fresh tree and never mutates its input), so reading one
	// never disturbs the other.
	AuthoringRoot LayoutNode `json:"-"`

	// LSMLAssets is the bundle-level asset block (allowedHosts/fonts/preload,
	// LSML §11 / 1.2 §5) carried from the authoring CanvasLayout straight to
	// EmitLSML, which preserves it on the LSML bundle (ADR 002 §3.4 T6 — the
	// host allowlist arms Solar's runtime gate). Distinct from `Assets`
	// above, which is the bespoke render bundle's content-addressed binary
	// AssetRef list. It is `json:"-"`: like AuthoringRoot it is an
	// LSML-emit-only carrier and never travels on the bespoke RenderBundle
	// wire nor enters its scene_version hash, so the served bespoke artefact
	// is byte-identical with or without it. Opaque json.RawMessage — Orion
	// neither fabricates nor strips it; nil when the layout authored no
	// assets.
	//
	// FIX 2026-06-29: serialise it as `assets` on the bespoke RenderBundle.
	// Solar's render-side host gate (`readAllowedHosts` → `gateSrc`, Bastion
	// T1/T2) reads `bundle.assets.allowedHosts` (OBJECT form) off the SAME
	// bespoke bundle it fetches in broadcast mode — NOT the LSML bundle. With
	// `assets` omitted the gate is deny-by-default and EVERY image primitive
	// returns null (the whole scene renders text/shapes but zero images). The
	// lumencast TS compiler already emits `assets: {allowedHosts,...}` here
	// (compile.ts); Orion now matches it. The content-addressed AssetRef list
	// moved to `asset_refs` (Solar never read it for the host allowlist).
	LSMLAssets json.RawMessage `json:"assets,omitempty"`
}
