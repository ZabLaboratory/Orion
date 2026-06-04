package compiler

import (
	"encoding/json"
	"fmt"

	"github.com/Lumencast/lumencast-go/lsml"
)

// LSMLVersion is the LSML major.minor this emitter targets. The
// lumencast-go validator only implements 1.0 (lsml.Version), but the
// bundle TYPE carries `lsml` as a free string and `layout` as opaque
// json.RawMessage, so a 1.1 bundle round-trips through the Go server as
// bytes — only the TS `@lumencast/compiler` (1.1-ready) interprets it.
// Per ADR 007 §5 (R3) the Go 1.0 validator is OFF the critical path:
// we never call lsml.Validate here.
const LSMLVersion = "1.1"

// EmitLSML maps Orion's compiled, fully-expanded render tree to an
// LSML 1.1 bundle (ADR 007 §C.1). It is ADDITIVE: it does not replace
// or mutate the bespoke RenderBundle path — `Compile` keeps producing
// the RenderBundle exactly as before. EmitLSML is a pure function the
// caller invokes separately when LSML output is wanted.
//
// Inputs are the post-step-6 artefacts `Compile` already builds
// (compile.go:81-117): the expanded LayoutNode tree, the hoisted
// operator inputs, the external adapters, and the operator-authored
// animation tree (opaque — Orion is a transport for it, Solar v0.2+
// interprets it). `animations` may be nil.
//
// It returns the sealed bundle, its LSML content address (the
// scene_version, formatted "sha256:<hex>"), and the canonical bytes
// `lsml.HashBundle` hashed. The bundle's scene_version is set to that
// address so the returned *lsml.Bundle is self-consistent.
//
// Determinism: the layout/bind/animate maps are marshalled via
// encoding/json (key-stable) and the whole bundle is canonicalised by
// lsml.HashBundle (sorted keys, no whitespace) — the same discipline
// Orion's computeSceneVersion already uses, so the hash is stable
// across calls on identical inputs (acceptance #2).
func EmitLSML(
	sceneID string,
	root LayoutNode,
	inputs []OperatorInput,
	adapters []ExternalAdapter,
	animations json.RawMessage,
) (*lsml.Bundle, string, []byte, error) {
	layout, err := lsmlNode(root, animations)
	if err != nil {
		return nil, "", nil, fmt.Errorf("emit lsml layout: %w", err)
	}

	bundle := &lsml.Bundle{
		LSML:             LSMLVersion,
		SceneID:          sceneID,
		Layout:           layout,
		OperatorInputs:   lsmlOperatorInputs(inputs),
		ExternalAdapters: lsmlExternalAdapters(adapters),
	}

	// HashBundle zeroes scene_version before hashing, so seeding it
	// here (or leaving it empty) does not affect the hash — but we set
	// the placeholder so the bundle is well-formed if marshalled before
	// sealing.
	hexHash, canon, err := lsml.HashBundle(bundle)
	if err != nil {
		return nil, "", nil, fmt.Errorf("emit lsml hash: %w", err)
	}
	version := "sha256:" + hexHash
	bundle.SceneVersion = version

	return bundle, version, canon, nil
}

// lsmlNode converts one Orion LayoutNode into an LSML node, encoded as
// raw JSON because lsml.Node has unexported Extra and cannot carry the
// arbitrary static props LSML places at the node's top level.
//
// Mapping (ADR 007 §C.1, verified against lumencast-go/lsml fixtures):
//
//	LayoutNode.Kind        → "kind"
//	LayoutNode.ID          → "id"          (omitted when empty)
//	LayoutNode.Props[k]    → "<k>"         (static props spread at top level)
//	LayoutNode.Bindings    → "bind"        (dynamic leaf-path bindings)
//	LayoutNode.Transitions → "animate"     (transform/opacity/filter)
//	LayoutNode.Children    → "children"
//
// `rootAnimations` is the bundle-level operator-authored animation tree;
// it is attached only to the root node under "animations" (opaque), and
// only when non-empty. Child nodes never carry it.
func lsmlNode(n LayoutNode, rootAnimations json.RawMessage) (json.RawMessage, error) {
	node := map[string]any{
		"kind": n.Kind,
	}
	if n.ID != "" {
		node["id"] = n.ID
	}

	// Static props are spread at the node's top level. A prop named
	// "kind"/"id"/"bind"/"animate"/"children"/"animations" would clash
	// with a reserved field; we never overwrite a reserved key.
	for k, raw := range n.Props {
		if isReservedNodeKey(k) {
			continue
		}
		node[k] = raw
	}

	if len(n.Bindings) > 0 {
		bind := make(map[string]string, len(n.Bindings))
		for k, v := range n.Bindings {
			bind[k] = v
		}
		node["bind"] = bind
	}

	if len(n.Transitions) > 0 {
		animate := make(map[string]json.RawMessage, len(n.Transitions))
		for k, v := range n.Transitions {
			animate[k] = v
		}
		node["animate"] = animate
	}

	if len(n.Children) > 0 {
		children := make([]json.RawMessage, 0, len(n.Children))
		for _, c := range n.Children {
			cj, err := lsmlNode(c, nil)
			if err != nil {
				return nil, err
			}
			children = append(children, cj)
		}
		node["children"] = children
	}

	// Operator-authored animation tree rides through opaque on the
	// root node (ADR 007 §C.1: "Animations rides through opaque as it
	// does today"). Orion never parses it.
	if len(rootAnimations) > 0 {
		node["animations"] = rootAnimations
	}

	return json.Marshal(node)
}

// isReservedNodeKey reports whether a prop name would collide with an
// LSML node structural field. Such props are dropped from the spread
// rather than silently corrupting the node shape.
func isReservedNodeKey(k string) bool {
	switch k {
	case "kind", "id", "bind", "animate", "children", "animations":
		return true
	}
	return false
}

// lsmlOperatorInputs maps Orion OperatorInputs to the LSML shape.
// Orion's richer constraint fields (Min/Max/Step/MaxLength/Regex/
// EnumValues/OptionsSrc) fold into LSML's generic Constraints map so
// nothing is lost in the bundle.
func lsmlOperatorInputs(inputs []OperatorInput) []lsml.OperatorInput {
	if len(inputs) == 0 {
		return nil
	}
	out := make([]lsml.OperatorInput, 0, len(inputs))
	for _, in := range inputs {
		oi := lsml.OperatorInput{
			Path:       in.Path,
			Label:      in.Label,
			Type:       in.Type,
			Group:      in.Group,
			WritableBy: in.WritableBy,
		}
		constraints := map[string]any{}
		if in.Min != nil {
			constraints["min"] = *in.Min
		}
		if in.Max != nil {
			constraints["max"] = *in.Max
		}
		if in.Step != nil {
			constraints["step"] = *in.Step
		}
		if in.MaxLength != nil {
			constraints["max_length"] = *in.MaxLength
		}
		if in.Regex != "" {
			constraints["regex"] = in.Regex
		}
		if in.OptionsSrc != "" {
			constraints["options_source"] = in.OptionsSrc
		}
		if len(constraints) > 0 {
			oi.Constraints = constraints
		}
		if len(in.EnumValues) > 0 {
			vals := make([]any, len(in.EnumValues))
			for i, v := range in.EnumValues {
				vals[i] = v
			}
			oi.Values = vals
		}
		out = append(out, oi)
	}
	return out
}

// lsmlExternalAdapters maps Orion's typed ExternalAdapter list to the
// LSML external_adapters block, which the lsml.Bundle carries as a slice
// of opaque json.RawMessage (§9). Orion remains the adapter executor;
// the bundle merely declares them.
func lsmlExternalAdapters(adapters []ExternalAdapter) []json.RawMessage {
	if len(adapters) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(adapters))
	for _, a := range adapters {
		raw, err := json.Marshal(a)
		if err != nil {
			// ExternalAdapter is a plain struct of JSON-safe fields;
			// marshalling cannot realistically fail. Skip on the
			// impossible error rather than aborting the whole emit.
			continue
		}
		out = append(out, raw)
	}
	return out
}
